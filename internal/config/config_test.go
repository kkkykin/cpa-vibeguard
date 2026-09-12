package config

import (
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHostStoreMetadata(t *testing.T) {
	store, err := os.ReadFile("../../testdata/plugin-store.yaml")
	if err != nil {
		t.Fatal(err)
	}
	base := "enabled: true\npriority: 100\non_request_error: passthrough\n"
	want, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, store string }{
		{"manifest", string(store)},
		{"host extension", strings.Replace(string(store), "store:", "store:\n  future: null\n  on_request_error: reject", 1)},
		{"empty metadata", "store: null\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse([]byte(base + tc.store))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("host metadata changed plugin configuration: got %+v, want %+v", got, want)
			}
		})
	}
	for _, invalid := range []string{
		"unexpected: true", "limits: {unknown: 1}", "on_request_error: null", "limits: {max_inflight: '1'}",
	} {
		t.Run(invalid, func(t *testing.T) {
			if _, err := Parse([]byte(string(store) + invalid)); err == nil {
				t.Fatal("store metadata disabled plugin configuration validation")
			}
		})
	}
}

func TestDefaultsAndStrictConfig(t *testing.T) {
	c, err := Parse(nil)
	if err != nil || c.OnUnknownAPI != "reject" || c.OnRequestError != "reject" || c.PreserveMarkerHint || c.Session != nil || c.Limits.RequestBytes != 8<<20 || c.Limits.MaxInflight != 256 || len(c.Bypass.Headers) != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = Parse([]byte("enabled: true\npriority: 100\nsession:\n  profiles:\n    - namespace: default\n      sources:\n        - metadata: /execution_session_id\n"))
	if err != nil || c.Session.TTL != Duration(30*time.Minute) || c.Session.Identity != "caller_scope" {
		t.Fatalf("%+v %v", c, err)
	}
	invalid := []string{
		"api: openai", "fail_open: true", "on_unknown_api: invalid", "preserve_marker_hint: 'true'", "limits: {request_bytes: 0}", "limits: {event_bytes: '1024'}", "limits: {unknown: 1}",
		"disabled_rules: [123]", "disabled_rules: [null]", "disabled_rules: ['']", "disabled_rules: null", "rule_sources: [{inline: 'rules: []'}]", "rule_sources: [{inline: []}]", "rule_sources: [{file: relative.yaml}]", "rule_sources: [{file: /tmp/config.json}]", "rule_sources: [{file: /tmp/config.yaml, inline: {}}]",
		"bypass: {headers: [{name: X-Skip}]}", "bypass: {headers: [{name: X-Skip, present: false}]}", "bypass: {headers: [{name: X-Skip, value: true}]}", "bypass: {headers: [{name: X-Skip, value: 'true', present: true}]}", "bypass: {headers: [{name: 'bad name', present: true}]}",
		"session: {identity: X-User}", "session: {ttl: 0s}", "session: {on_missing: passthrough}", "session: {profiles: [{namespace: x, sources: [{env: SECRET}]}]}", "session: {profiles: [{namespace: x, sources: [{json: /previous_response_id}]}]}",
		"variables: {v: {header: X-V, json: /v}}", "variables: {v: {json: /bad~2}}", "variables: {v: {metadata: x}}", "variables: {v: {env: ''}}", "priority: 1\npriority: 2", "priority: 1\n---\npriority: 2",
	}
	for _, raw := range invalid {
		t.Run(raw, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}

func TestBypassExactHeaders(t *testing.T) {
	c, err := Parse([]byte("bypass:\n  headers:\n    - name: X-Skip\n      value: 'true'\n    - name: X-Present\n      present: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    bool
	}{
		{"absent", nil, false}, {"case", http.Header{"x-sKip": {"true"}}, true}, {"wrong value", http.Header{"X-Skip": {"True"}}, false}, {"space", http.Header{"X-Skip": {" true"}}, false}, {"comma", http.Header{"X-Skip": {"true,true"}}, false}, {"multi", http.Header{"X-Skip": {"true", "true"}}, false}, {"cased duplicate", http.Header{"X-Skip": {"true"}, "x-skip": {"true"}}, false}, {"present empty", http.Header{"X-Present": {}}, true}, {"present any", http.Header{"X-Present": {"false", "yes"}}, true}, {"or", http.Header{"X-Skip": {"false"}, "X-Present": {""}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Bypass.Matches(tc.headers); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestInlinePreservesRepresentation(t *testing.T) {
	c, err := Parse([]byte("rule_sources:\n  - inline:\n      extend: {useDefault: false}\n      rules:\n        - id: demo\n          regex: '\\b\\d{2}\\b'\n          keywords: []\n          secretGroup: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := c.RuleSources[0].Inline["rules"].([]any)[0].(map[string]any)
	if r["regex"] != `\b\d{2}\b` {
		t.Fatal(r)
	}
	if _, ok := r["secretGroup"]; !ok {
		t.Fatal("explicit zero lost")
	}
	if _, ok := r["entropy"]; ok {
		t.Fatal("omitted field gained a value")
	}
}
