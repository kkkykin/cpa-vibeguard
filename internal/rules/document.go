// Package rules merges Gitleaks-compatible documents and creates immutable
// detectors. Protocols never participate in rule compilation or matching.
package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/kkkykin/cpa-vibeguard/internal/config"
	"github.com/kkkykin/cpa-vibeguard/internal/document"
)

const MaxSourceBytes = 4 << 20
const MaxSources = 64
const MaxTotalSourceBytes = 16 << 20
const MaxRules = 4096

// Pointer fields retain the distinction between omission and explicit zero,
// false, or empty arrays throughout source merging.
type RuleDocument struct {
	Title       *string     `json:"title,omitempty"`
	Description *string     `json:"description,omitempty"`
	MinVersion  *string     `json:"minVersion,omitempty"`
	Extend      *Extend     `json:"extend,omitempty"`
	Rules       []Rule      `json:"rules,omitempty"`
	Allowlist   *Allowlist  `json:"allowlist,omitempty"`
	Allowlists  []Allowlist `json:"allowlists,omitempty"`
	Origin      string      `json:"-"`
}
type Extend struct {
	UseDefault    *bool    `json:"useDefault,omitempty"`
	DisabledRules []string `json:"disabledRules,omitempty"`
}
type Rule struct {
	ID          string      `json:"id"`
	Description *string     `json:"description,omitempty"`
	Regex       *string     `json:"regex,omitempty"`
	SecretGroup *int        `json:"secretGroup,omitempty"`
	Entropy     *float64    `json:"entropy,omitempty"`
	Keywords    *[]string   `json:"keywords,omitempty"`
	Tags        *[]string   `json:"tags,omitempty"`
	Allowlist   *Allowlist  `json:"allowlist,omitempty"`
	Allowlists  []Allowlist `json:"allowlists,omitempty"`
	Origin      string      `json:"-"`
}
type Allowlist struct {
	Description string   `json:"description,omitempty"`
	Condition   string   `json:"condition,omitempty"`
	RegexTarget string   `json:"regexTarget,omitempty"`
	Regexes     []string `json:"regexes,omitempty"`
	Stopwords   []string `json:"stopwords,omitempty"`
	TargetRules []string `json:"targetRules,omitempty"`
}

func ParseYAML(raw []byte, origin string) (RuleDocument, error) {
	if len(raw) > MaxSourceBytes {
		return RuleDocument{}, fmt.Errorf("%s: rule_source_size_limit", origin)
	}
	m, err := document.YAML(raw)
	if err != nil {
		return RuleDocument{}, fmt.Errorf("%s: %w", origin, err)
	}
	return ParseDocument(m, origin)
}
func ParseTOML(raw []byte, origin string) (RuleDocument, error) {
	if len(raw) > MaxSourceBytes {
		return RuleDocument{}, fmt.Errorf("%s: rule_source_size_limit", origin)
	}
	var m map[string]any
	if _, err := toml.Decode(string(raw), &m); err != nil {
		return RuleDocument{}, fmt.Errorf("%s: invalid_toml", origin)
	}
	if err := tomlValues(m); err != nil {
		return RuleDocument{}, fmt.Errorf("%s: %w", origin, err)
	}
	return ParseDocument(m, origin)
}
func tomlValues(v any) error {
	switch x := v.(type) {
	case string, bool, int64, float64:
	case map[string]any:
		for _, v := range x {
			if err := tomlValues(v); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range x {
			if err := tomlValues(v); err != nil {
				return err
			}
		}
	case []map[string]any:
		for _, v := range x {
			if err := tomlValues(v); err != nil {
				return err
			}
		}
	default:
		return errors.New("unsupported_toml_value_type")
	}
	return nil
}
func noNull(v any) error {
	if v == nil {
		return errors.New("null_rule_field")
	}
	switch x := v.(type) {
	case map[string]any:
		for _, v := range x {
			if err := noNull(v); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range x {
			if err := noNull(v); err != nil {
				return err
			}
		}
	}
	return nil
}
func ParseDocument(m map[string]any, origin string) (RuleDocument, error) {
	fail := func(err error) (RuleDocument, error) { return RuleDocument{}, fmt.Errorf("%s: %w", origin, err) }
	if err := noNull(m); err != nil {
		return fail(err)
	}
	raw, err := json.Marshal(m)
	if err != nil || len(raw) > MaxSourceBytes {
		return fail(errors.New("rule_source_size_limit"))
	}
	var d RuleDocument
	if err := document.Decode(m, &d); err != nil {
		return fail(err)
	}
	d.Origin = origin
	if d.MinVersion != nil {
		v := strings.TrimPrefix(*d.MinVersion, "v")
		var major, minor, patch int
		if _, err := fmt.Sscanf(v, "%d.%d.%d", &major, &minor, &patch); err != nil || fmt.Sprintf("%d.%d.%d", major, minor, patch) != v || major != 8 || minor > 30 || (minor == 30 && patch > 1) {
			return fail(errors.New("unsupported_minVersion"))
		}
	}
	if d.Allowlist != nil {
		if len(d.Allowlists) > 0 {
			return fail(errors.New("allowlist_and_allowlists_conflict"))
		}
		d.Allowlists = append(d.Allowlists, *d.Allowlist)
		d.Allowlist = nil
	}
	if len(d.Rules) > MaxRules {
		return fail(errors.New("rule_count_limit"))
	}
	seen := make(map[string]bool)
	for i := range d.Rules {
		r := &d.Rules[i]
		r.Origin = origin
		if r.ID == "" || strings.TrimSpace(r.ID) != r.ID || len(r.ID) > 256 {
			return fail(fmt.Errorf("rules[%d]: invalid_rule_id", i))
		}
		if seen[r.ID] {
			return fail(fmt.Errorf("rule %s: duplicate_rule_id", r.ID))
		}
		seen[r.ID] = true
		if r.Allowlist != nil {
			if len(r.Allowlists) > 0 {
				return fail(fmt.Errorf("rule %s: allowlist_and_allowlists_conflict", r.ID))
			}
			r.Allowlists = append(r.Allowlists, *r.Allowlist)
			r.Allowlist = nil
		}
		if err := validateRuleFields(*r); err != nil {
			return fail(fmt.Errorf("rule %s: %w", r.ID, err))
		}
		for _, a := range r.Allowlists {
			if len(a.TargetRules) > 0 {
				return fail(fmt.Errorf("rule %s: targetRules_requires_global_allowlist", r.ID))
			}
		}
	}
	for i, a := range d.Allowlists {
		if err := validateAllowlist(a); err != nil {
			return fail(fmt.Errorf("allowlists[%d]: %w", i, err))
		}
	}
	if d.Extend != nil {
		for _, id := range d.Extend.DisabledRules {
			if id == "" {
				return fail(errors.New("empty_disabled_rule"))
			}
		}
	}
	return d, nil
}

func LoadDocuments(sources []config.RuleSource) ([]RuleDocument, error) {
	if len(sources) > MaxSources {
		return nil, errors.New("rule_source_count_limit")
	}
	docs := make([]RuleDocument, 0, len(sources))
	total := 0
	for i, s := range sources {
		var d RuleDocument
		var err error
		if s.Inline != nil {
			raw, _ := json.Marshal(s.Inline)
			total += len(raw)
			d, err = ParseDocument(s.Inline, fmt.Sprintf("inline[%d]", i))
		} else {
			f, openErr := os.Open(*s.File)
			if openErr != nil {
				return nil, fmt.Errorf("%s: rule_source_read_error", *s.File)
			}
			raw, readErr := io.ReadAll(io.LimitReader(f, MaxSourceBytes+1))
			total += len(raw)
			closeErr := f.Close()
			if readErr != nil || closeErr != nil {
				return nil, fmt.Errorf("%s: rule_source_read_error", *s.File)
			}
			switch strings.ToLower(filepath.Ext(*s.File)) {
			case ".toml":
				d, err = ParseTOML(raw, *s.File)
			case ".yaml", ".yml":
				d, err = ParseYAML(raw, *s.File)
			default:
				err = fmt.Errorf("%s: unsupported_extension", *s.File)
			}
		}
		if err != nil {
			return nil, err
		}
		if total > MaxTotalSourceBytes {
			return nil, errors.New("rule_sources_total_size_limit")
		}
		docs = append(docs, d)
	}
	return docs, nil
}

func mergeRule(base, next Rule) Rule {
	if next.Description != nil {
		base.Description = next.Description
	}
	if next.Regex != nil {
		base.Regex = next.Regex
	}
	if next.SecretGroup != nil {
		base.SecretGroup = next.SecretGroup
	}
	if next.Entropy != nil {
		base.Entropy = next.Entropy
	}
	if next.Keywords != nil {
		base.Keywords = next.Keywords
	}
	if next.Tags != nil {
		base.Tags = next.Tags
	}
	base.Allowlists = append(append([]Allowlist(nil), base.Allowlists...), next.Allowlists...)
	base.Origin = next.Origin
	return base
}
