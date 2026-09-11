package rules

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
)

type reference struct{ kind, name string }
type templatePart struct {
	literal string
	ref     *reference
}
type template struct {
	parts []templatePart
	regex bool
}

// Parse templates before binding values. Escapes, Unicode classes, counted
// repetitions, and character classes retain their regular-expression meaning.
func parseTemplate(raw string, isRegex bool) (template, error) {
	t := template{regex: isRegex}
	start := 0
	inClass := false
	quoted := false
	for i := 0; i < len(raw); i++ {
		if isRegex && raw[i] == '\\' {
			if i+1 == len(raw) {
				return t, errors.New("unfinished_regex_escape")
			}
			if raw[i+1] == 'Q' {
				quoted = true
			}
			if raw[i+1] == 'E' {
				quoted = false
			}
			i++
			continue
		}
		if isRegex && !quoted {
			if raw[i] == '[' {
				inClass = true
			}
			if raw[i] == ']' {
				inClass = false
			}
		}
		if raw[i] != '{' {
			continue
		}
		kind := ""
		if strings.HasPrefix(raw[i:], "{env.") {
			kind = "env"
		}
		if strings.HasPrefix(raw[i:], "{var.") {
			kind = "var"
		}
		if kind == "" {
			continue
		}
		if inClass || quoted {
			return t, errors.New("variable_in_regex_class_or_quote")
		}
		end := strings.IndexByte(raw[i:], '}')
		if end < 0 {
			return t, errors.New("unfinished_variable")
		}
		end += i
		name := raw[i+5 : end]
		if !config.ValidName(name) {
			return t, errors.New("invalid_template_variable")
		}
		if i > start {
			t.parts = append(t.parts, templatePart{literal: raw[start:i]})
		}
		t.parts = append(t.parts, templatePart{ref: &reference{kind: kind, name: name}})
		i = end
		start = end + 1
	}
	if start < len(raw) {
		t.parts = append(t.parts, templatePart{literal: raw[start:]})
	}
	return t, nil
}
func (t template) expand(values map[reference]string, placeholder bool) (string, error) {
	var out strings.Builder
	for _, p := range t.parts {
		if p.ref == nil {
			out.WriteString(p.literal)
			continue
		}
		v, ok := values[*p.ref]
		if placeholder {
			v = "VGBindingPlaceholder"
			ok = true
		}
		if !ok {
			return "", errors.New("variable_missing")
		}
		if t.regex {
			out.WriteString("(?:")
			out.WriteString(regexp.QuoteMeta(v))
			out.WriteString(")")
		} else {
			out.WriteString(v)
		}
	}
	return out.String(), nil
}
func (t template) references() []reference {
	var refs []reference
	for _, p := range t.parts {
		if p.ref != nil {
			refs = append(refs, *p.ref)
		}
	}
	return refs
}

func validateRegex(raw string, group *int, secret bool) error {
	t, err := parseTemplate(raw, true)
	if err != nil {
		return err
	}
	expanded, _ := t.expand(nil, true)
	re, err := regexp.Compile(expanded)
	if err != nil {
		return errors.New("invalid_regex")
	}
	ast, err := syntax.Parse(expanded, syntax.Perl)
	if err != nil {
		return errors.New("invalid_regex")
	}
	if secret && minBytes(ast) == 0 {
		return errors.New("empty_match_regex")
	}
	if group != nil {
		if *group < 0 || *group > re.NumSubexp() {
			return errors.New("invalid_secretGroup")
		}
		if *group > 0 && captureNullable(ast, *group) {
			return errors.New("empty_secretGroup")
		}
	}
	return nil
}
func minBytes(r *syntax.Regexp) int {
	switch r.Op {
	case syntax.OpLiteral:
		return len(r.Rune)
	case syntax.OpCharClass, syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return 1
	case syntax.OpCapture, syntax.OpPlus:
		return minBytes(r.Sub[0])
	case syntax.OpRepeat:
		return r.Min * minBytes(r.Sub[0])
	case syntax.OpConcat:
		n := 0
		for _, s := range r.Sub {
			n += minBytes(s)
		}
		return n
	case syntax.OpAlternate:
		n := int(^uint(0) >> 1)
		for _, s := range r.Sub {
			if v := minBytes(s); v < n {
				n = v
			}
		}
		return n
	default:
		return 0
	}
}
func captureNullable(r *syntax.Regexp, group int) bool {
	if r.Op == syntax.OpCapture && r.Cap == group {
		return minBytes(r.Sub[0]) == 0
	}
	for _, s := range r.Sub {
		if captureNullable(s, group) {
			return true
		}
	}
	return false
}
func validateRuleFields(r Rule) error {
	if r.SecretGroup != nil && *r.SecretGroup < 0 {
		return errors.New("invalid_secretGroup")
	}
	if r.Entropy != nil && (*r.Entropy < 0 || *r.Entropy > 8) {
		return errors.New("invalid_entropy")
	}
	if r.Regex != nil {
		if err := validateRegex(*r.Regex, r.SecretGroup, true); err != nil {
			return fmt.Errorf("regex: %w", err)
		}
	}
	if r.Keywords != nil {
		for _, v := range *r.Keywords {
			if v == "" {
				return errors.New("empty_keyword")
			}
			if _, err := parseTemplate(v, false); err != nil {
				return fmt.Errorf("keywords: %w", err)
			}
		}
	}
	for i, a := range r.Allowlists {
		if err := validateAllowlist(a); err != nil {
			return fmt.Errorf("allowlists[%d]: %w", i, err)
		}
	}
	return nil
}
func validateAllowlist(a Allowlist) error {
	if len(a.Regexes) == 0 && len(a.Stopwords) == 0 {
		return errors.New("empty_allowlist")
	}
	switch strings.ToUpper(a.Condition) {
	case "", "OR", "AND", "||", "&&":
	default:
		return errors.New("invalid_allowlist_condition")
	}
	switch a.RegexTarget {
	case "", "secret", "match", "line":
	default:
		return errors.New("invalid_regexTarget")
	}
	for _, v := range a.Regexes {
		if err := validateRegex(v, nil, false); err != nil {
			return fmt.Errorf("regexes: %w", err)
		}
	}
	for _, v := range a.Stopwords {
		if v == "" {
			return errors.New("empty_stopword")
		}
		if _, err := parseTemplate(v, false); err != nil {
			return fmt.Errorf("stopwords: %w", err)
		}
	}
	for _, id := range a.TargetRules {
		if id == "" {
			return errors.New("empty_target_rule")
		}
	}
	return nil
}
