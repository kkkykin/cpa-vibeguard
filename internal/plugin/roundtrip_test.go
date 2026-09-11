package plugin

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/document"
	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const privateRule = `rule_sources:
  - inline:
      extend: {useDefault: false}
      rules:
        - {id: fixture, regex: PRIVATE}
`

func TestAllProtocolRoundTrips(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/protocols.json")
	if err != nil {
		t.Fatal(err)
	}
	var fs []struct {
		Name, Format      string
		Request, Response json.RawMessage
	}
	if err := json.Unmarshal(raw, &fs); err != nil {
		t.Fatal(err)
	}
	e := configured(t, privateRule)
	for _, f := range fs {
		t.Run(f.Name, func(t *testing.T) {
			// This test covers independent request scopes. History bindings have
			// separate session tests and require an authenticated session.
			v, _ := document.JSON(f.Request)
			root := v.(map[string]any)
			delete(root, "previous_response_id")
			delete(root, "previous_interaction_id")
			requestRaw, _ := json.Marshal(root)
			req := pluginapi.RequestInterceptRequest{RequestID: f.Name, SourceFormat: f.Format, ToFormat: "different-upstream-format", Body: requestRaw}
			r := e.Before(req)
			if r.Terminate || len(r.Body) == 0 {
				t.Fatalf("redaction failed: %+v %s", r, r.ResponseBody)
			}
			masked, err := protocol.Parse(f.Format, r.Body, true, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			marker := ""
			for _, field := range masked.Fields {
				if !token.Valid(field.Text) {
					t.Fatalf("plaintext escaped %s: %s", field.Path, field.Text)
				}
				if marker == "" {
					marker = field.Text
				}
				if marker != field.Text {
					t.Fatal("same plaintext did not reuse marker")
				}
			}
			if after := e.After(pluginapi.RequestInterceptRequest{RequestID: f.Name, SourceFormat: f.Format, Body: r.Body, ToFormat: "changed-credential"}); after.Terminate || len(after.Body) > 0 {
				t.Fatal("after-auth hook changed committed request")
			}
			response, err := protocol.Parse(f.Format, f.Response, false, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range response.Fields {
				field.Set(marker)
			}
			responseRaw, err := response.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			out := e.Response(pluginapi.ResponseInterceptRequest{RequestID: f.Name, SourceFormat: f.Format, Body: responseRaw})
			if len(out.Body) == 0 {
				t.Fatal("response not restored")
			}
			expected, err := protocol.Parse(f.Format, f.Response, false, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			expectedRaw, _ := expected.Marshal()
			a, _ := document.JSON(out.Body)
			b, _ := document.JSON(expectedRaw)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("got %s expected %s", out.Body, expectedRaw)
			}
			e.Complete(f.Name)
			e.Complete(f.Name)
			if stats := e.mappings.Stats(); stats.Entries != 0 || stats.Leases != 0 {
				t.Fatal(stats)
			}
		})
	}
}

func TestIdempotentRetriesAndConfigurationSnapshots(t *testing.T) {
	e := configured(t, "preserve_marker_hint: true\n"+privateRule)
	req := pluginapi.RequestInterceptRequest{RequestID: "retry", SourceFormat: "openai", Body: []byte(`{"messages":[{"role":"user","content":"PRIVATE"}]}`)}
	first := e.Before(req)
	if first.Terminate {
		t.Fatal(first)
	}
	if err := e.Configure([]byte("disabled_rules: [fixture]\n" + privateRule)); err != nil {
		t.Fatal(err)
	}
	req.Body = []byte(`{"messages":[{"role":"user","content":"different"}]}`)
	if next := e.Before(req); !bytes.Equal(next.Body, first.Body) {
		t.Fatal("retry regenerated marker or instruction")
	}
	if strings.Count(string(first.Body), "Preserve privacy markers exactly as written") != 1 {
		t.Fatal("duplicate hint")
	}
	doc, _ := protocol.Parse("openai", first.Body, true, 1<<20)
	marker := doc.Fields[len(doc.Fields)-1].Text
	resp := e.Response(pluginapi.ResponseInterceptRequest{RequestID: "retry", SourceFormat: "openai", Body: []byte(`{"choices":[{"message":{"content":"` + marker + `"}}]}`)})
	if !bytes.Contains(resp.Body, []byte(`"content":"PRIVATE"`)) {
		t.Fatal("reconfigure lost existing restoration mapping")
	}
	e.Complete("retry")
	req.RequestID = "new"
	req.Body = []byte(`{"messages":[{"role":"user","content":"PRIVATE"}]}`)
	if next := e.Before(req); !bytes.Contains(next.Body, []byte("PRIVATE")) || bytes.Contains(next.Body, []byte(token.Prefix)) {
		t.Fatal("disabled rule still active for new request")
	}
}

func TestFailureRollbackAndStickyPassthrough(t *testing.T) {
	e := configured(t, "on_request_error: passthrough\nlimits: {request_state_bytes: 60}\n"+privateRule)
	req := pluginapi.RequestInterceptRequest{RequestID: "rollback", SourceFormat: "openai", Body: []byte(`{"messages":[{"role":"user","content":"PRIVATE"}]}`)}
	if out := e.Before(req); out.Terminate || len(out.Body) > 0 {
		t.Fatal("failed request did not preserve original hook body")
	}
	if stats := e.mappings.Stats(); stats.Entries != 0 || stats.Scopes != 0 {
		t.Fatal("failed request published mappings", stats)
	}
	if err := e.Configure([]byte(privateRule)); err != nil {
		t.Fatal(err)
	}
	if next := e.Before(req); next.Terminate || len(next.Body) > 0 {
		t.Fatal("error passthrough decision changed on retry")
	}
	if s := e.Stats(); s.RequestErrorPassthrough != 1 || s.UnknownPassthrough != 0 {
		t.Fatal(s)
	}
	e.Complete("rollback")
	missing := configured(t, "on_unknown_api: passthrough\nvariables: {v: {header: X-V}}\nrule_sources: [{inline: {extend: {useDefault: false}, rules: [{id: v, regex: '{var.v}'}]}}]")
	if out := missing.Before(req); !out.Terminate {
		t.Fatal("binding error incorrectly became unknown API passthrough")
	}
}

func TestScopeIsolationAndResponseDegradation(t *testing.T) {
	e := configured(t, privateRule)
	body := []byte(`{"messages":[{"role":"user","content":"PRIVATE"}]}`)
	a := e.Before(pluginapi.RequestInterceptRequest{RequestID: "a", TraceID: "shared", SourceFormat: "openai", Body: body})
	b := e.Before(pluginapi.RequestInterceptRequest{RequestID: "b", TraceID: "shared", SourceFormat: "openai", Body: body})
	if bytes.Equal(a.Body, b.Body) {
		t.Fatal("TraceID shared mappings across independent requests")
	}
	doc, _ := protocol.Parse("openai", a.Body, true, 1024)
	marker := doc.Fields[0].Text
	response := []byte(`{"choices":[{"message":{"content":"` + marker + `"}}]}`)
	out := e.Response(pluginapi.ResponseInterceptRequest{RequestID: "b", SourceFormat: "openai", Body: response})
	if !bytes.Contains(out.Body, []byte(marker)) {
		t.Fatal("cross-request marker restored")
	}
	for i := 0; i < 2; i++ {
		out = e.Response(pluginapi.ResponseInterceptRequest{RequestID: "a", SourceFormat: "claude", Body: response})
		if len(out.Body) > 0 {
			t.Fatal("mismatched format rewritten")
		}
	}
	if e.Stats().ResponseDegraded != 1 {
		t.Fatal("degradation counted repeatedly")
	}
	if out := e.Response(pluginapi.ResponseInterceptRequest{RequestID: "missing", SourceFormat: "openai", Body: response}); len(out.Body) > 0 {
		t.Fatal("missing state searched another scope")
	}
}

func TestOrdinaryResponseExpansionIsBoundedAndTransactional(t *testing.T) {
	e := configured(t, "limits: {response_bytes: 512}\nrule_sources: [{inline: {extend: {useDefault: false}, rules: [{id: long, regex: 'SECRET-[a-z]+'}]}}]")
	secret := "SECRET-" + strings.Repeat("x", 400)
	request, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": secret}}})
	r := e.Before(pluginapi.RequestInterceptRequest{RequestID: "expansion", SourceFormat: "openai", Body: request})
	if r.Terminate {
		t.Fatal(r)
	}
	doc, err := protocol.Parse("openai", r.Body, true, 1024)
	if err != nil {
		t.Fatal(err)
	}
	marker := doc.Fields[0].Text
	// Each field independently fits, but their combined plaintext exceeds
	// the per-response budget. Never publish a partially restored document.
	raw := []byte(`{"choices":[{"message":{"content":"` + marker + `","refusal":"` + marker + `"}}]}`)
	for i := 0; i < 2; i++ {
		out := e.Response(pluginapi.ResponseInterceptRequest{RequestID: "expansion", SourceFormat: "openai", Body: raw})
		if len(out.Body) != 0 || !bytes.Contains(raw, []byte(marker)) || bytes.Contains(raw, []byte(secret)) {
			t.Fatal("response overflow committed a partial rewrite")
		}
	}
	if e.Stats().ResponseDegraded != 1 {
		t.Fatal("response overflow was counted more than once")
	}
	valid := []byte(`{"choices":[{"message":{"content":"` + marker + `"}}]}`)
	out := e.Response(pluginapi.ResponseInterceptRequest{RequestID: "expansion", SourceFormat: "openai", Body: valid})
	if len(out.Body) == 0 || !bytes.Contains(out.Body, []byte(secret)) || len(out.Body) > 512 {
		t.Fatal("valid bounded response did not restore")
	}
}
