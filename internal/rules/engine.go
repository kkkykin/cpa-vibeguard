package rules

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/kkkykin/cpa-vibeguard/internal/config"
	"github.com/kkkykin/cpa-vibeguard/internal/document"
	"github.com/rs/zerolog"
	glconfig "github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/logging"
	glregexp "github.com/zricethezav/gitleaks/v8/regexp"
)

const MaxVariableBytes = 4096
const MaxBindingBytes = 64 << 10
const CacheEntries = 64
const CacheBytes = 32 << 20
const CacheTTL = 10 * time.Minute

// Gitleaks' diagnostic logs can contain findings and expanded patterns. Disable
// its private logger once, before any detector exists; plugin outcomes use codes.
func init() { logging.Logger = zerolog.Nop() }

var builtinOnce sync.Once
var builtinConfig glconfig.Config
var builtinError error

func builtins() (glconfig.Config, error) {
	builtinOnce.Do(func() {
		var v glconfig.ViperConfig
		if _, err := toml.Decode(glconfig.DefaultConfig, &v); err != nil {
			builtinError = errors.New("builtin_decode_error")
			return
		}
		c, err := v.Translate()
		if err != nil {
			builtinError = errors.New("builtin_validation_error")
			return
		}
		builtinConfig = c
	})
	return builtinConfig, builtinError
}

type rulePlan struct {
	rule        Rule
	base        glconfig.Rule
	regex       template
	keywords    []template
	allows      []*allowPlan
	staticRegex *glregexp.Regexp
}
type allowPlan struct {
	value              Allowlist
	regexes, stopwords []template
	static             *glconfig.Allowlist
	staticWeight       int
}
type cacheEntry struct {
	bound *Bound
	used  time.Time
	bytes int
}
type Engine struct {
	plans       []*rulePlan
	global      []*glconfig.Allowlist
	refs        []reference
	environment map[reference]string
	variables   map[string]config.Source
	key         [32]byte
	mu          sync.Mutex
	cache       map[[32]byte]cacheEntry
	cacheBytes  int
	now         func() time.Time
	static      *Bound
}
type Input struct {
	Headers  http.Header
	JSON     any
	Metadata map[string]any
}
type Bound struct {
	detector  *detect.Detector
	originals map[string]string
	rules     map[string]glconfig.Rule
	weight    int
}
type CacheStats struct{ Entries, Bytes int }

func New(cfg *config.Config, lookup func(string) (string, bool)) (*Engine, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	docs, err := LoadDocuments(cfg.RuleSources)
	if err != nil {
		return nil, err
	}
	useDefault := true
	disabled := make(map[string]bool)
	for _, id := range cfg.DisabledRules {
		disabled[id] = true
	}
	for _, d := range docs {
		if d.Extend != nil {
			if d.Extend.UseDefault != nil {
				useDefault = *d.Extend.UseDefault
			}
			for _, id := range d.Extend.DisabledRules {
				disabled[id] = true
			}
		}
	}
	merged := make(map[string]Rule)
	baseRules := make(map[string]glconfig.Rule)
	e := &Engine{variables: cfg.Variables, environment: make(map[reference]string), cache: make(map[[32]byte]cacheEntry), now: time.Now}
	if _, err := rand.Read(e.key[:]); err != nil {
		return nil, errors.New("cache_key_random_error")
	}
	if useDefault {
		c, err := builtins()
		if err != nil {
			return nil, err
		}
		e.global = c.Allowlists
		for id, base := range c.Rules {
			baseRules[id] = base
			r := Rule{ID: id, Description: ptr(base.Description), SecretGroup: ptr(base.SecretGroup), Entropy: ptr(base.Entropy), Keywords: ptr(append([]string(nil), base.Keywords...)), Tags: ptr(append([]string(nil), base.Tags...)), Origin: "builtin"}
			if base.Regex != nil {
				r.Regex = ptr(base.Regex.String())
			}
			merged[id] = r
		}
	}
	var global []Allowlist
	for _, d := range docs {
		for _, r := range d.Rules {
			if old, ok := merged[r.ID]; ok {
				merged[r.ID] = mergeRule(old, r)
			} else {
				if r.Regex == nil {
					return nil, fmt.Errorf("%s: rule %s: missing_rule_for_override", d.Origin, r.ID)
				}
				merged[r.ID] = r
			}
		}
		global = append(global, d.Allowlists...)
	}
	if len(merged) > MaxRules {
		return nil, errors.New("rule_count_limit")
	}
	ids := make([]string, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Validate references and merged rules before disabling anything.
	for _, id := range ids {
		r := merged[id]
		if err := validateRuleFields(r); err != nil {
			return nil, fmt.Errorf("%s: rule %s: %w", r.Origin, id, err)
		}
	}
	for id := range disabled {
		if id == "" {
			return nil, errors.New("empty_disabled_rule")
		}
		if _, ok := merged[id]; !ok {
			return nil, fmt.Errorf("disabled_rules: unknown_rule_id %s", id)
		}
	}
	for _, a := range global {
		for _, id := range a.TargetRules {
			if _, ok := merged[id]; !ok {
				return nil, fmt.Errorf("allowlists: unknown_target_rule %s", id)
			}
		}
	}
	refs := make(map[reference]bool)
	for _, id := range ids {
		if disabled[id] {
			continue
		}
		r := merged[id]
		p := &rulePlan{rule: r, base: baseRules[id]}
		if r.Regex != nil {
			p.regex, _ = parseTemplate(*r.Regex, true)
			if len(p.regex.references()) == 0 {
				p.staticRegex, err = regexp.Compile(*r.Regex)
				if err != nil {
					return nil, fmt.Errorf("rule %s: invalid_regex", id)
				}
			}
		}
		if r.Keywords != nil {
			for _, v := range *r.Keywords {
				t, _ := parseTemplate(v, false)
				p.keywords = append(p.keywords, t)
			}
		}
		allows := append([]Allowlist(nil), r.Allowlists...)
		for _, a := range global {
			if len(a.TargetRules) == 0 || contains(a.TargetRules, id) {
				allows = append(allows, a)
			}
		}
		for _, a := range allows {
			p.allows = append(p.allows, planAllow(a))
		}
		for _, t := range append([]template{p.regex}, p.keywords...) {
			for _, ref := range t.references() {
				refs[ref] = true
			}
		}
		for _, a := range p.allows {
			for _, t := range append(append([]template(nil), a.regexes...), a.stopwords...) {
				for _, ref := range t.references() {
					refs[ref] = true
				}
			}
		}
		e.plans = append(e.plans, p)
	}
	for ref := range refs {
		e.refs = append(e.refs, ref)
	}
	sort.Slice(e.refs, func(i, j int) bool {
		a, b := e.refs[i], e.refs[j]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.name < b.name
	})
	dynamic := false
	total := 0
	for _, ref := range e.refs {
		name := ""
		if ref.kind == "env" {
			name = ref.name
		} else {
			source, ok := cfg.Variables[ref.name]
			if !ok {
				return nil, fmt.Errorf("variable %s: undeclared_variable", ref.name)
			}
			if source.Env != nil {
				name = *source.Env
			} else {
				dynamic = true
				continue
			}
		}
		v, ok := lookup(name)
		if !ok || v == "" {
			return nil, fmt.Errorf("variable %s: environment_missing", ref.name)
		}
		if len(v) > MaxVariableBytes {
			return nil, fmt.Errorf("variable %s: variable_size_limit", ref.name)
		}
		total += len(v)
		e.environment[ref] = v
	}
	if total > MaxBindingBytes {
		return nil, errors.New("binding_size_limit")
	}
	// Compile all parts that only depend on configuration-time environment
	// values now. A changing header must not recompile unrelated patterns.
	for _, p := range e.plans {
		if p.rule.Regex != nil && p.staticRegex == nil && resolved(p.regex, e.environment) {
			expr, _ := p.regex.expand(e.environment, false)
			p.staticRegex, err = regexp.Compile(expr)
			if err != nil {
				return nil, fmt.Errorf("rule %s: invalid_bound_regex", p.rule.ID)
			}
		}
		for _, a := range p.allows {
			ready := true
			for _, t := range append(append([]template(nil), a.regexes...), a.stopwords...) {
				if !resolved(t, e.environment) {
					ready = false
				}
			}
			if ready {
				compiled, weight, err := a.compile(e.environment)
				if err != nil {
					return nil, fmt.Errorf("rule %s: %w", p.rule.ID, err)
				}
				a.static, a.staticWeight = compiled, weight
			}
		}
	}
	if !dynamic {
		e.static, err = e.compile(e.environment)
		if err != nil {
			return nil, err
		}
	}
	return e, nil
}
func resolved(t template, values map[reference]string) bool {
	for _, ref := range t.references() {
		if _, ok := values[ref]; !ok {
			return false
		}
	}
	return true
}
func ptr[T any](v T) *T { return &v }
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if s == v {
			return true
		}
	}
	return false
}
func planAllow(a Allowlist) *allowPlan {
	p := &allowPlan{value: a}
	for _, v := range a.Regexes {
		t, _ := parseTemplate(v, true)
		p.regexes = append(p.regexes, t)
	}
	for _, v := range a.Stopwords {
		t, _ := parseTemplate(v, false)
		p.stopwords = append(p.stopwords, t)
	}
	return p
}

func (e *Engine) Bind(input Input) (*Bound, error) {
	if e.static != nil {
		return e.static, nil
	}
	values := make(map[reference]string, len(e.refs))
	total := 0
	for _, ref := range e.refs {
		v, ok := e.environment[ref]
		if !ok {
			s := e.variables[ref.name]
			var value any
			var exists bool
			var err error
			switch {
			case s.Header != nil:
				vs, present := config.HeaderValues(input.Headers, *s.Header)
				if present && len(vs) != 1 {
					return nil, fmt.Errorf("variable %s: ambiguous_header", ref.name)
				}
				if len(vs) == 1 {
					value = vs[0]
					exists = true
				}
			case s.JSON != nil:
				value, exists, err = document.Pointer(input.JSON, *s.JSON)
			case s.Metadata != nil:
				value, exists, err = document.Pointer(input.Metadata, *s.Metadata)
			default:
				err = errors.New("invalid_variable_source")
			}
			if err != nil {
				return nil, fmt.Errorf("variable %s: invalid_variable_source", ref.name)
			}
			var isString bool
			v, isString = value.(string)
			if !exists || !isString || v == "" {
				return nil, fmt.Errorf("variable %s: missing_or_invalid_value", ref.name)
			}
		}
		if len(v) > MaxVariableBytes {
			return nil, fmt.Errorf("variable %s: variable_size_limit", ref.name)
		}
		total += len(v)
		if total > MaxBindingBytes {
			return nil, errors.New("binding_size_limit")
		}
		values[ref] = v
	}
	h := hmac.New(sha256.New, e.key[:])
	var size [8]byte
	for _, ref := range e.refs {
		v := values[ref]
		binary.BigEndian.PutUint64(size[:], uint64(len(v)))
		h.Write(size[:])
		h.Write([]byte(v))
	}
	var key [32]byte
	copy(key[:], h.Sum(nil))
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for k, v := range e.cache {
		if now.Sub(v.used) >= CacheTTL {
			delete(e.cache, k)
			e.cacheBytes -= v.bytes
		}
	}
	if v, ok := e.cache[key]; ok {
		v.used = now
		e.cache[key] = v
		return v.bound, nil
	}
	b, err := e.compile(values)
	if err != nil {
		return nil, err
	}
	if b.weight > CacheBytes {
		return nil, errors.New("compiled_rule_size_limit")
	}
	for len(e.cache) >= CacheEntries || e.cacheBytes+b.weight > CacheBytes {
		var oldestKey [32]byte
		var oldest time.Time
		for k, v := range e.cache {
			if oldest.IsZero() || v.used.Before(oldest) {
				oldest = v.used
				oldestKey = k
			}
		}
		v := e.cache[oldestKey]
		delete(e.cache, oldestKey)
		e.cacheBytes -= v.bytes
	}
	e.cache[key] = cacheEntry{bound: b, used: now, bytes: b.weight}
	e.cacheBytes += b.weight
	return b, nil
}
func (e *Engine) Stats() CacheStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return CacheStats{Entries: len(e.cache), Bytes: e.cacheBytes}
}
func (e *Engine) RuleIDs() []string {
	ids := make([]string, len(e.plans))
	for i, p := range e.plans {
		ids[i] = p.rule.ID
	}
	return ids
}

func (e *Engine) compile(values map[reference]string) (*Bound, error) {
	c := glconfig.Config{Rules: make(map[string]glconfig.Rule), Keywords: make(map[string]struct{}), Allowlists: e.global}
	b := &Bound{originals: make(map[string]string), rules: make(map[string]glconfig.Rule), weight: 1024}
	for i, p := range e.plans {
		r := p.base
		r.RuleID = fmt.Sprintf("vg%04d", i)
		// Neutral runtime IDs disable Gitleaks' cross-finding generic-rule
		// suppression, which otherwise loses separate occurrences on one line.
		b.originals[r.RuleID] = p.rule.ID
		if p.rule.Description != nil {
			r.Description = *p.rule.Description
		}
		if p.rule.SecretGroup != nil {
			r.SecretGroup = *p.rule.SecretGroup
		}
		if p.rule.Entropy != nil {
			r.Entropy = *p.rule.Entropy
		}
		if p.rule.Tags != nil {
			r.Tags = append([]string(nil), (*p.rule.Tags)...)
		}
		if p.rule.Regex != nil {
			expr, err := p.regex.expand(values, false)
			if err != nil {
				return nil, err
			}
			r.Regex = p.staticRegex
			if r.Regex == nil {
				r.Regex, err = regexp.Compile(expr)
				if err != nil {
					return nil, fmt.Errorf("rule %s: invalid_bound_regex", p.rule.ID)
				}
			}
			b.weight += len(expr)*64 + 512
		}
		r.Keywords = nil
		for _, t := range p.keywords {
			v, err := t.expand(values, false)
			if err != nil {
				return nil, err
			}
			v = strings.ToLower(v)
			r.Keywords = append(r.Keywords, v)
			c.Keywords[v] = struct{}{}
			b.weight += len(v)*64 + 64
		}
		r.Allowlists = append([]*glconfig.Allowlist(nil), p.base.Allowlists...)
		for _, a := range p.allows {
			compiled, weight, err := a.compile(values)
			if err != nil {
				return nil, fmt.Errorf("rule %s: %w", p.rule.ID, err)
			}
			r.Allowlists = append(r.Allowlists, compiled)
			b.weight += weight
		}
		// Default path-only rules stay path-only; text fragments never pretend
		// to be repository files. User documents cannot add repository rules.
		c.Rules[r.RuleID] = r
		b.rules[r.RuleID] = r
		c.OrderedRules = append(c.OrderedRules, r.RuleID)
	}
	if b.weight > CacheBytes {
		return nil, errors.New("compiled_rule_size_limit")
	}
	b.detector = detect.NewDetector(c)
	b.detector.IgnoreGitleaksAllow = true
	b.detector.MaxDecodeDepth = 0
	return b, nil
}
func (p *allowPlan) compile(values map[reference]string) (*glconfig.Allowlist, int, error) {
	if p.static != nil {
		return p.static, p.staticWeight, nil
	}
	a := &glconfig.Allowlist{Description: p.value.Description, RegexTarget: p.value.RegexTarget}
	if strings.EqualFold(p.value.Condition, "AND") || p.value.Condition == "&&" {
		a.MatchCondition = glconfig.AllowlistMatchAnd
	}
	static := true
	weight := 256
	for _, t := range p.regexes {
		v, err := t.expand(values, false)
		if err != nil {
			return nil, 0, err
		}
		r, err := regexp.Compile(v)
		if err != nil {
			return nil, 0, errors.New("invalid_bound_allowlist_regex")
		}
		a.Regexes = append(a.Regexes, r)
		weight += len(v)*64 + 128
		if len(t.references()) > 0 {
			static = false
		}
	}
	for _, t := range p.stopwords {
		v, err := t.expand(values, false)
		if err != nil {
			return nil, 0, err
		}
		a.StopWords = append(a.StopWords, v)
		weight += len(v)*64 + 64
		if len(t.references()) > 0 {
			static = false
		}
	}
	if a.Validate() != nil {
		return nil, 0, errors.New("invalid_bound_allowlist")
	}
	// Called under the engine cache mutex, or during construction before
	// publication. Static allowlists are published once and then read-only.
	if static {
		p.static = a
		p.staticWeight = weight
	}
	return a, weight, nil
}

func (b *Bound) Scan(ctx context.Context, text string) ([]Span, error) { return b.scan(ctx, text) }
