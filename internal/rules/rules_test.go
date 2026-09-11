package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
)

func testEngine(t *testing.T, raw string) *Engine {
	t.Helper()
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func scan(t *testing.T, e *Engine, text string, input Input) []Span {
	t.Helper()
	b, err := e.Bind(input)
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Scan(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func custom(rules string) string {
	return "rule_sources:\n  - inline:\n      extend: {useDefault: false}\n      rules:\n" + rules
}

func TestEquivalentDocuments(t *testing.T) {
	yaml := []byte(`extend:
  useDefault: false
  disabledRules: [disabled]
allowlists:
  - targetRules: [demo]
    regexTarget: match
    regexes: ['allow']
rules:
  - id: demo
    description: fixture
    regex: '\b(SECRET-[A-Z]+)\b'
    secretGroup: 1
    entropy: 0
    keywords: [SECRET-]
    tags: [test]
    allowlists:
      - condition: AND
        regexes: ['{var.allowed}']
        stopwords: [allow]
  - id: disabled
    regex: unused
`)
	toml := []byte(`[extend]
useDefault = false
disabledRules = ["disabled"]
[[allowlists]]
targetRules = ["demo"]
regexTarget = "match"
regexes = ['allow']
[[rules]]
id = "demo"
description = "fixture"
regex = '\b(SECRET-[A-Z]+)\b'
secretGroup = 1
entropy = 0
keywords = ["SECRET-"]
tags = ["test"]
[[rules.allowlists]]
condition = "AND"
regexes = ['{var.allowed}']
stopwords = ['allow']
[[rules]]
id = "disabled"
regex = "unused"
`)
	a, err := ParseYAML(yaml, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseTOML(toml, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("TOML and YAML differ:\n%+v\n%+v", a, b)
	}
	var sourceCfg config.Config
	for _, ext := range []string{".TOML", ".YAML", ".yml"} {
		data := yaml
		if ext == ".TOML" {
			data = toml
		}
		path := filepath.Join(t.TempDir(), "rules"+ext)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		sourceCfg = config.Defaults()
		sourceCfg.RuleSources = []config.RuleSource{{File: &path}}
		sourceCfg.Variables = map[string]config.Source{"allowed": {Header: ptr("X-Allow")}}
		e, err := New(&sourceCfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s := scan(t, e, "SECRET-YES", Input{Headers: http.Header{"X-Allow": {"no"}}}); len(s) != 1 || s[0].Start != 0 || s[0].End != 10 {
			t.Fatal(s)
		}
	}
}

func TestStrictRuleDocuments(t *testing.T) {
	invalid := []string{
		"rules: [{id: a, regex: 1}]", "rules: [{id: a, regex: true}]", "rules: [{id: a, regex: null}]", "rules: [{id: a, regex: foo, keywords: [2]}]",
		"rules: [{id: a, regex: x}, {id: a, regex: y}]", "rules: [{id: a, regex: x, required: []}]", "rules: [{id: a, regex: x, skipReport: false}]", "rules: [{id: a, regex: x, path: x}]",
		"extend: {path: /etc/rules.toml}", "extend: {url: 'https://example.com/rules'}", "extend: {use_default: false}", "extend: {useDefault: 'false'}",
		"rules: [{id: a, regex: x, secret_group: 1}]", "rules: [{id: a, regex: x, secretGroup: 1}]", "rules: [{id: a, regex: 'x('}]", "rules: [{id: a, regex: 'x*'}]", "rules: [{id: a, regex: '\\b'}]", "rules: [{id: a, regex: 'a(b*)', secretGroup: 1}]",
		"rules: [{id: a, regex: '[{var.x}]'}]", "rules: [{id: a, regex: '{var.x'}]", "rules: [{id: a, regex: '{var.a-b}'}]",
		"rules: [{id: a, regex: x, allowlists: [{paths: [x]}]}]", "allowlists: [{commits: [x]}]", "allowlists: [{regexTarget: invalid, regexes: [x]}]", "allowlists: [{condition: invalid, regexes: [x]}]", "allowlists: [{regexes: []}]",
		"allowlist: {regexes: [x]}\nallowlists: [{regexes: [y]}]", "rules: [{id: a, regex: x, allowlist: {regexes: [x]}, allowlists: [{regexes: [y]}]}]",
		"rules: [{id: a, regex: x, allowlists: [{regexes: [x], targetRules: [a]}]}]", "rules: []\nrules: []", "rules: []\n---\nrules: []",
	}
	for _, raw := range invalid {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseYAML([]byte(raw), "fixture"); err == nil {
				t.Fatal("invalid document accepted")
			}
		})
	}
	for _, raw := range []string{"[[rules]]\nid='x'\nregex=2026-01-01", "[[rules]]\nid='x'\nregex=1", "[[rules]]\nid='x'\nregex='x'\nregex='y'"} {
		if _, err := ParseTOML([]byte(raw), "fixture"); err == nil {
			t.Fatal("invalid TOML accepted")
		}
	}
}

func TestMergeExplicitFieldsAndDisableUnion(t *testing.T) {
	e := testEngine(t, `disabled_rules: [second]
rule_sources:
  - inline:
      extend: {useDefault: false, disabledRules: [first]}
      rules:
        - {id: first, regex: '{env.NEVER_REQUIRED}'}
        - {id: second, regex: '{var.undeclared}'}
        - {id: active, regex: '(prefix)-(SECRET)', secretGroup: 2, entropy: 4, keywords: [absent]}
  - inline:
      extend: {disabledRules: []}
      rules:
        - {id: first, regex: first}
        - {id: second, regex: second}
        - {id: active, secretGroup: 0, entropy: 0, keywords: []}
`)
	if ids := e.RuleIDs(); !reflect.DeepEqual(ids, []string{"active"}) {
		t.Fatal(ids)
	}
	spans := scan(t, e, "prefix-SECRET first second", Input{})
	if len(spans) != 1 || spans[0].Start != 0 || spans[0].End != 6 {
		t.Fatal(spans)
	}
	// Gitleaks group 0 chooses the first non-empty group; the explicit zero
	// must replace inherited group 2, and empty keywords remove prefiltering.
	for _, raw := range []string{
		"disabled_rules: [UNKNOWN]", "disabled_rules: [GENERIC-API-KEY]",
		custom("        - {id: a, allowlists: [{regexes: [x]}]}\n"),
		"disabled_rules: [a]\n" + custom("        - {id: a, regex: '('}\n"),
		custom("        - {id: a, regex: x}\n") + "      allowlists: [{regexes: [x], targetRules: [missing]}]\n",
	} {
		cfg, err := config.Parse([]byte(raw))
		if err == nil {
			_, err = New(cfg, nil)
		}
		if err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestBuiltinsAndDisabledBuiltins(t *testing.T) {
	e := testEngine(t, "")
	if len(e.RuleIDs()) < 150 {
		t.Fatalf("default rules not loaded: %d", len(e.RuleIDs()))
	}
	text := "github = ghp_0123456789abcdef0123456789ABCDEFGHijkl"
	if s := scan(t, e, text, Input{}); len(s) == 0 {
		t.Fatal("default github rule did not detect fixture")
	}
	e = testEngine(t, "disabled_rules: [github-pat]")
	for _, s := range scan(t, e, text, Input{}) {
		if contains(s.RuleIDs, "github-pat") {
			t.Fatal(s)
		}
	}
}

func TestExactSpansAndOverlap(t *testing.T) {
	for _, tc := range []struct {
		name, regex, text string
		group             int
		start, end        int
	}{
		{"capture duplicate", `(ITEM_A): (ITEM_A)`, "ITEM_A: ITEM_A", 2, 8, 14},
		{"UTF8 CRLF", `(ITEM_A): (ITEM_A)`, "中文\r\nITEM_A: ITEM_A", 2, 16, 22},
		{"multiline", `BEGIN\r?\n(中文\r?\nSECRET)`, "x\nBEGIN\r\n中文\r\nSECRET\r\nz", 1, 9, 23},
		{"repeated context", `key=(SECRET)`, "SECRET before key=SECRET", 1, 18, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := testEngine(t, custom(fmt.Sprintf("        - {id: test, regex: '%s', secretGroup: %d}\n", tc.regex, tc.group)))
			s := scan(t, e, tc.text, Input{})
			if len(s) != 1 || s[0].Start != tc.start || s[0].End != tc.end {
				t.Fatalf("got %+v, expected %d:%d", s, tc.start, tc.end)
			}
		})
	}
	e := testEngine(t, custom("        - {id: left, regex: ABCD}\n        - {id: right, regex: CDEF}\n        - {id: middle, regex: BC}\n"))
	s := scan(t, e, "ABCDEF", Input{})
	if len(s) != 1 || s[0].Start != 0 || s[0].End != 6 || !reflect.DeepEqual(s[0].RuleIDs, []string{"left", "middle", "right"}) {
		t.Fatal(s)
	}
}

func TestGitleaksCommentAndGenericSuppression(t *testing.T) {
	e := testEngine(t, custom("        - {id: generic-test, regex: SECRET}\n        - {id: contextual, regex: 'key=(SECRET)', secretGroup: 1}\n"))
	text := "SECRET and key=SECRET # gitleaks:allow"
	s := scan(t, e, text, Input{})
	if len(s) != 2 || s[0].Start != 0 || s[1].Start != 15 {
		t.Fatal(s)
	}
	if s := scan(t, e, "U0VDUkVU %53%45%43%52%45%54", Input{}); len(s) != 0 {
		t.Fatal("decoded hidden content unexpectedly")
	}
}

func TestVariablesAreLiteralAndSnapshot(t *testing.T) {
	value := "a.*[b](c)\"\n\\${2}中文"
	t.Setenv("VG_TEST_SECRET", value)
	e := testEngine(t, custom("        - id: env\n          regex: '(prefix):({env.VG_TEST_SECRET})'\n          secretGroup: 2\n          keywords: ['{env.VG_TEST_SECRET}']\n"))
	t.Setenv("VG_TEST_SECRET", "changed")
	s := scan(t, e, "prefix:"+value, Input{})
	if len(s) != 1 || s[0].Start != 7 || s[0].End != 7+len(value) {
		t.Fatal(s)
	}
	if s := scan(t, e, "prefix:changed", Input{}); len(s) != 0 {
		t.Fatal(s)
	}
	e = testEngine(t, "variables: {v: {header: X-Tenant}}\n"+custom("        - {id: v, regex: '\\p{Han}{2}({var.v})', secretGroup: 1, keywords: ['{var.v}']}\n"))
	s = scan(t, e, "中文"+value, Input{Headers: http.Header{"X-Tenant": {value}}})
	if len(s) != 1 || s[0].Start != 6 || s[0].End != 6+len(value) {
		t.Fatal(s)
	}
	for _, h := range []http.Header{nil, {"X-Tenant": {""}}, {"X-Tenant": {"a", "b"}}, {"X-Tenant": {strings.Repeat("x", MaxVariableBytes+1)}}} {
		if _, err := e.Bind(Input{Headers: h}); err == nil {
			t.Fatal("invalid variable accepted")
		}
	}
}

func TestDynamicAllowlistOnStaticRule(t *testing.T) {
	e := testEngine(t, `variables: {allowed: {json: /a~1b/0}}
rule_sources:
  - inline:
      extend: {useDefault: false}
      rules:
        - {id: secret, regex: 'SECRET-[A-Z]+'}
      allowlists:
        - targetRules: [secret]
          regexes: ['^{var.allowed}$']
`)
	for _, v := range []string{"SECRET-ONE", "SECRET-TWO", "SECRET-ONE"} {
		s := scan(t, e, "SECRET-ONE SECRET-TWO", Input{JSON: map[string]any{"a/b": []any{v}}})
		if len(s) != 1 || ("SECRET-ONE SECRET-TWO"[s[0].Start:s[0].End]) == v {
			t.Fatal(s)
		}
	}
	for _, v := range []any{nil, 1, true, ""} {
		if _, err := e.Bind(Input{JSON: map[string]any{"a/b": []any{v}}}); err == nil {
			t.Fatal("invalid JSON variable accepted")
		}
	}
}

func TestDisabledAllowlistDependenciesPruned(t *testing.T) {
	e := testEngine(t, `disabled_rules: [off]
rule_sources:
  - inline:
      extend: {useDefault: false}
      rules:
        - id: off
          regex: '{env.NOT_REQUIRED}'
          allowlists: [{regexes: ['{var.not_declared}']}]
        - {id: on, regex: SECRET}
      allowlists:
        - {targetRules: [off], regexes: ['{env.NOT_REQUIRED_EITHER}']}
`)
	if s := scan(t, e, "SECRET", Input{}); len(s) != 1 {
		t.Fatal(s)
	}
	cfg, err := config.Parse([]byte(`disabled_rules: [off]
rule_sources:
  - inline:
      extend: {useDefault: false}
      rules:
        - {id: off, regex: '{env.NOT_REQUIRED}'}
        - {id: on, regex: '{env.NOT_REQUIRED}'}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("shared enabled dependency pruned")
	}
}

func TestCacheLimitsAndConcurrentSnapshots(t *testing.T) {
	e := testEngine(t, "variables: {v: {header: X-V}}\n"+custom("        - {id: v, regex: '{var.v}'}\n"))
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	for i := 0; i < CacheEntries+5; i++ {
		scan(t, e, fmt.Sprint(i), Input{Headers: http.Header{"X-V": {fmt.Sprint(i)}}})
	}
	if s := e.Stats(); s.Entries != CacheEntries || s.Bytes > CacheBytes {
		t.Fatal(s)
	}
	now = now.Add(CacheTTL + time.Second)
	scan(t, e, "fresh", Input{Headers: http.Header{"X-V": {"fresh"}}})
	if e.Stats().Entries != 1 {
		t.Fatal("expired cache entries retained")
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Go(func() {
			v := fmt.Sprintf("value-%03d", i)
			b, err := e.Bind(Input{Headers: http.Header{"X-V": {v}}})
			if err != nil {
				t.Error(err)
				return
			}
			s, err := b.Scan(context.Background(), v+" other")
			if err != nil || len(s) != 1 || s[0].End != len(v) {
				t.Errorf("cross-request contamination: %v %v", s, err)
			}
		})
	}
	wg.Wait()
}

func TestCancellationAndErrorPrivacy(t *testing.T) {
	e := testEngine(t, custom("        - {id: a, regex: SECRET}\n"))
	b, _ := e.Bind(Input{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Scan(ctx, "SECRET"); err == nil {
		t.Fatal("canceled scan became no findings")
	}
	_, err := ParseYAML([]byte("rules: [{id: safe-id, regex: 'DO-NOT-PRINT('}]"), "source.yaml")
	if err == nil || strings.Contains(err.Error(), "DO-NOT-PRINT") {
		t.Fatal(err)
	}
	t.Setenv("VG_CONF_SECRET", "EXPANDED-PRIVATE-VALUE")
	cfg, _ := config.Parse([]byte(custom("        - {id: a, regex: '{env.VG_CONF_SECRET}'}\n")))
	if _, err := New(cfg, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cfg)
	if strings.Contains(string(raw), "EXPANDED-PRIVATE-VALUE") || !strings.Contains(string(raw), "{env.VG_CONF_SECRET}") {
		t.Fatal("configuration mutated during expansion")
	}
}

func TestStaticCompilationReusedAcrossBindings(t *testing.T) {
	t.Setenv("VG_STATIC_PART", "CONFIG-SECRET")
	e := testEngine(t, "variables: {allow: {header: X-Allow}}\n"+custom("        - id: static\n          regex: '{env.VG_STATIC_PART}'\n          allowlists: [{stopwords: ['{var.allow}']}]\n"))
	if e.plans[0].staticRegex == nil {
		t.Fatal("environment-only regex not compiled at configuration time")
	}
	a, err := e.Bind(Input{Headers: http.Header{"X-Allow": {"one"}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Bind(Input{Headers: http.Header{"X-Allow": {"two"}}})
	if err != nil {
		t.Fatal(err)
	}
	if a.rules["vg0000"].Regex != b.rules["vg0000"].Regex {
		t.Fatal("static regex recompiled for an unrelated variable")
	}
}

func TestLegacyAllowlistsAndMixedSourceOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base.toml")
	if err := os.WriteFile(path, []byte("[extend]\nuseDefault=false\n[[rules]]\nid='x'\nregex='SECRET-[A-Z]+'\n[rules.allowlist]\nstopwords=['FIRST']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.RuleSources = []config.RuleSource{{File: &path}, {Inline: map[string]any{"rules": []any{map[string]any{"id": "x", "allowlists": []any{map[string]any{"stopwords": []any{"SECOND"}}}}}}}}
	e, err := New(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := "SECRET-FIRST SECRET-SECOND SECRET-THIRD"
	spans := scan(t, e, text, Input{})
	if len(spans) != 1 || text[spans[0].Start:spans[0].End] != "SECRET-THIRD" {
		t.Fatal(spans)
	}
	cfg.RuleSources = append(cfg.RuleSources, config.RuleSource{Inline: map[string]any{"extend": map[string]any{"useDefault": true}}})
	e, err = New(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.RuleIDs()) < 150 {
		t.Fatal("last explicit useDefault ignored")
	}
}
