// Package config owns immutable, strictly validated configuration snapshots.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kkkykin/cpa-vibeguard/internal/document"
)

const MaxConfigBytes = 4 << 20

type Config struct {
	Enabled            *bool             `json:"enabled,omitempty"`
	Priority           int               `json:"priority,omitempty"`
	OnUnknownAPI       string            `json:"on_unknown_api"`
	OnRequestError     string            `json:"on_request_error"`
	PreserveMarkerHint bool              `json:"preserve_marker_hint"`
	Bypass             Bypass            `json:"bypass"`
	Limits             Limits            `json:"limits"`
	Variables          map[string]Source `json:"variables"`
	DisabledRules      []string          `json:"disabled_rules"`
	RuleSources        []RuleSource      `json:"rule_sources"`
	Session            *Session          `json:"session,omitempty"`
}

type Bypass struct {
	Headers []HeaderCondition `json:"headers"`
}
type HeaderCondition struct {
	Name    string  `json:"name"`
	Value   *string `json:"value,omitempty"`
	Present *bool   `json:"present,omitempty"`
}
type Limits struct {
	RequestBytes      int `json:"request_bytes"`
	ResponseBytes     int `json:"response_bytes"`
	EventBytes        int `json:"event_bytes"`
	ToolJSONBytes     int `json:"tool_json_bytes"`
	RequestStateBytes int `json:"request_state_bytes"`
	MaxInflight       int `json:"max_inflight"`
}
type RuleSource struct {
	File   *string        `json:"file,omitempty"`
	Inline map[string]any `json:"inline,omitempty"`
}
type Source struct {
	Env      *string `json:"env,omitempty"`
	Header   *string `json:"header,omitempty"`
	JSON     *string `json:"json,omitempty"`
	Metadata *string `json:"metadata,omitempty"`
}
type Session struct {
	Identity             string    `json:"identity"`
	OnMissing            string    `json:"on_missing"`
	MaxIDBytes           int       `json:"max_id_bytes"`
	TTL                  Duration  `json:"ttl"`
	MaxSessions          int       `json:"max_sessions"`
	MaxEntriesPerSession int       `json:"max_entries_per_session"`
	MaxTotalBytes        int       `json:"max_total_bytes"`
	Profiles             []Profile `json:"profiles"`
}
type Profile struct {
	Namespace string   `json:"namespace"`
	Match     Match    `json:"match"`
	Sources   []Source `json:"sources"`
}
type Match struct {
	SourceFormat         string `json:"source_format,omitempty"`
	RequestedModelPrefix string `json:"requested_model_prefix,omitempty"`
}
type Duration time.Duration

func (d *Duration) UnmarshalJSON(raw []byte) error {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return errors.New("duration_requires_string")
	}
	t, err := time.ParseDuration(value)
	if err != nil {
		return errors.New("invalid_duration")
	}
	*d = Duration(t)
	return nil
}
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func Defaults() Config {
	return Config{OnUnknownAPI: "reject", OnRequestError: "reject",
		Limits:    Limits{RequestBytes: 8 << 20, ResponseBytes: 8 << 20, EventBytes: 1 << 20, ToolJSONBytes: 1 << 20, RequestStateBytes: 16 << 20, MaxInflight: 256},
		Variables: make(map[string]Source), DisabledRules: []string{}, RuleSources: []RuleSource{}, Bypass: Bypass{Headers: []HeaderCondition{}},
	}
}
func DefaultSession() Session {
	return Session{Identity: "caller_scope", OnMissing: "request", MaxIDBytes: 1024, TTL: Duration(30 * time.Minute), MaxSessions: 1024, MaxEntriesPerSession: 4096, MaxTotalBytes: 64 << 20}
}

func Parse(raw []byte) (*Config, error) {
	if len(raw) > MaxConfigBytes {
		return nil, errors.New("config_size_limit")
	}
	cfg := Defaults()
	if len(bytes.TrimSpace(raw)) == 0 {
		return &cfg, nil
	}
	m, err := document.YAML(raw)
	if err != nil {
		return nil, err
	}
	// Explicit null is not an omission in the configuration language.
	if err := rejectNull(m, "config"); err != nil {
		return nil, err
	}
	if _, ok := m["session"]; ok {
		s := DefaultSession()
		cfg.Session = &s
	}
	if err := document.Decode(m, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func rejectNull(v any, path string) error {
	if v == nil {
		return fmt.Errorf("null_field: %s", path)
	}
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			if err := rejectNull(v, path+"."+k); err != nil {
				return err
			}
		}
	case []any:
		for i, v := range x {
			if err := rejectNull(v, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ValidName(name string) bool { return variableName.MatchString(name) }
func HeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return true
}

func (c *Config) Validate() error {
	if c.OnUnknownAPI != "reject" && c.OnUnknownAPI != "passthrough" {
		return errors.New("invalid_on_unknown_api")
	}
	if c.OnRequestError != "reject" && c.OnRequestError != "passthrough" {
		return errors.New("invalid_on_request_error")
	}
	for _, n := range []int{c.Limits.RequestBytes, c.Limits.ResponseBytes, c.Limits.EventBytes, c.Limits.ToolJSONBytes, c.Limits.RequestStateBytes, c.Limits.MaxInflight} {
		if n < 1 || n > 1<<30 {
			return errors.New("invalid_limits")
		}
	}
	for i, h := range c.Bypass.Headers {
		if !HeaderName(h.Name) || (h.Value == nil) == (h.Present == nil) || (h.Present != nil && !*h.Present) {
			return fmt.Errorf("invalid_bypass_condition[%d]", i)
		}
	}
	for name, source := range c.Variables {
		if !ValidName(name) {
			return errors.New("invalid_variable_name")
		}
		if err := source.Validate(true); err != nil {
			return fmt.Errorf("variable %s: %w", name, err)
		}
	}
	for i, s := range c.RuleSources {
		if (s.File == nil) == (s.Inline == nil) {
			return fmt.Errorf("invalid_rule_source[%d]", i)
		}
		if s.File != nil {
			if !filepath.IsAbs(*s.File) {
				return fmt.Errorf("rule_source[%d]: absolute_path_required", i)
			}
			switch strings.ToLower(filepath.Ext(*s.File)) {
			case ".toml", ".yaml", ".yml":
			default:
				return fmt.Errorf("rule_source[%d]: unsupported_extension", i)
			}
		}
	}
	for _, id := range c.DisabledRules {
		if id == "" {
			return errors.New("empty_disabled_rule")
		}
	}
	if s := c.Session; s != nil {
		if s.Identity != "caller_scope" {
			return errors.New("invalid_session_identity")
		}
		if s.OnMissing != "request" && s.OnMissing != "reject" {
			return errors.New("invalid_session_on_missing")
		}
		if s.MaxIDBytes < 1 || s.MaxIDBytes > 1<<20 || s.TTL <= 0 || s.MaxSessions < 1 || s.MaxEntriesPerSession < 1 || s.MaxTotalBytes < 1 {
			return errors.New("invalid_session_limits")
		}
		for i, p := range s.Profiles {
			if p.Namespace == "" || strings.TrimSpace(p.Namespace) != p.Namespace || len(p.Namespace) > 1024 || len(p.Sources) == 0 {
				return fmt.Errorf("invalid_session_profile[%d]", i)
			}
			for _, source := range p.Sources {
				if err := source.Validate(false); err != nil {
					return fmt.Errorf("session_profile[%d]: %w", i, err)
				}
				if source.JSON != nil {
					parts, _ := document.PointerParts(*source.JSON)
					if len(parts) > 0 && (parts[len(parts)-1] == "previous_response_id" || parts[len(parts)-1] == "previous_interaction_id") {
						return errors.New("history_reference_is_not_session_source")
					}
				}
			}
		}
	}
	return nil
}
func (s Source) Validate(allowEnv bool) error {
	n := 0
	for _, p := range []*string{s.Env, s.Header, s.JSON, s.Metadata} {
		if p != nil {
			n++
		}
	}
	if n != 1 {
		return errors.New("source_requires_one_selector")
	}
	if s.Env != nil && (!allowEnv || !ValidName(*s.Env)) {
		return errors.New("invalid_env_source")
	}
	if s.Header != nil && !HeaderName(*s.Header) {
		return errors.New("invalid_header_source")
	}
	for _, p := range []*string{s.JSON, s.Metadata} {
		if p != nil {
			if _, err := document.PointerParts(*p); err != nil {
				return err
			}
		}
	}
	return nil
}

// HeaderValues includes differently cased map entries, preserving ambiguity.
func HeaderValues(headers http.Header, name string) ([]string, bool) {
	var values []string
	present := false
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			present = true
			values = append(values, v...)
		}
	}
	return values, present
}
func (b Bypass) Matches(headers http.Header) bool {
	for _, h := range b.Headers {
		values, present := HeaderValues(headers, h.Name)
		if h.Present != nil && *h.Present && present {
			return true
		}
		if h.Value != nil && len(values) == 1 && values[0] == *h.Value {
			return true
		}
	}
	return false
}
