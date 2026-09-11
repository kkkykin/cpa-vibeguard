package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/document"
	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const scopedConfig = `session:
  profiles:
    - namespace: default
      sources:
        - header: X-Session
        - json: /metadata/session_id
        - metadata: /execution_session_id
`

func TestSessionHistoricalPrefixAndSourceSwitch(t *testing.T) {
	e := configured(t, "preserve_marker_hint: true\n"+scopedConfig+privateRule)
	first := pluginapi.RequestInterceptRequest{RequestID: "one", SourceFormat: "openai", Headers: http.Header{"X-Session": {"session-1"}}, Metadata: map[string]any{"caller_scope": "caller"}, Body: []byte(`{"messages":[{"role":"user","content":"PRIVATE"}]}`)}
	a := e.Before(first)
	if a.Terminate {
		t.Fatal(string(a.ResponseBody))
	}
	e.Complete("one")
	second := first
	second.RequestID = "two"
	second.Headers = nil
	second.Body = []byte(`{"metadata":{"session_id":"session-1"},"messages":[{"role":"user","content":"PRIVATE"},{"role":"assistant","content":"answer"},{"role":"user","content":"PRIVATE again"}]}`)
	b := e.Before(second)
	if b.Terminate {
		t.Fatal(string(b.ResponseBody))
	}
	aTree, _ := document.JSON(a.Body)
	bTree, _ := document.JSON(b.Body)
	aMessages := aTree.(map[string]any)["messages"].([]any)
	bMessages := bTree.(map[string]any)["messages"].([]any)
	x, _ := json.Marshal(aMessages)
	y, _ := json.Marshal(bMessages[:len(aMessages)])
	if !bytes.Equal(x, y) {
		t.Fatalf("unchanged history prefix changed:\n%s\n%s", x, y)
	}
	e.Complete("two")
	second.RequestID = "foreign"
	second.Metadata = map[string]any{"caller_scope": "other"}
	c := e.Before(second)
	if c.Terminate {
		t.Fatal(c)
	}
	cTree, _ := document.JSON(c.Body)
	cMessages := cTree.(map[string]any)["messages"].([]any)
	z, _ := json.Marshal(cMessages[:len(aMessages)])
	if bytes.Equal(x, z) {
		t.Fatal("identity isolation failed")
	}
}

func TestContinuationBindingsAcrossReconfigure(t *testing.T) {
	for _, format := range []string{"openai-response", "interactions"} {
		t.Run(format, func(t *testing.T) {
			e := configured(t, scopedConfig+privateRule)
			req := pluginapi.RequestInterceptRequest{RequestID: "first", SourceFormat: format, Headers: http.Header{"X-Session": {"same"}}, Metadata: map[string]any{"caller_scope": "caller"}, Body: []byte(`{"input":"PRIVATE"}`)}
			masked := e.Before(req)
			if masked.Terminate {
				t.Fatal(string(masked.ResponseBody))
			}
			doc, err := protocol.Parse(format, masked.Body, true, 1024)
			if err != nil {
				t.Fatal(err)
			}
			marker := doc.Fields[0].Text
			response := `{"id":"ref-1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + marker + `"}]}]}`
			previous := "previous_response_id"
			if format == "interactions" {
				response = `{"id":"ref-1","steps":[{"type":"model_output","content":[{"type":"text","text":"` + marker + `"}]}]}`
				previous = "previous_interaction_id"
			}
			out := e.Response(pluginapi.ResponseInterceptRequest{RequestID: "first", SourceFormat: format, Body: []byte(response)})
			if !bytes.Contains(out.Body, []byte("PRIVATE")) {
				t.Fatal("response not restored")
			}
			e.Complete("first")
			if err := e.Configure([]byte("disabled_rules: [fixture]\n" + scopedConfig + privateRule)); err != nil {
				t.Fatal(err)
			}
			req.RequestID = "continued"
			req.Headers = nil
			req.Body = []byte(fmt.Sprintf(`{"input":"new turn","%s":"ref-1"}`, previous))
			continued := e.Before(req)
			if continued.Terminate {
				t.Fatal(string(continued.ResponseBody))
			}
			out = e.Response(pluginapi.ResponseInterceptRequest{RequestID: "continued", SourceFormat: format, Body: []byte(response)})
			if !bytes.Contains(out.Body, []byte("PRIVATE")) {
				t.Fatal("disabled rules destroyed old mapping")
			}
			e.Complete("continued")
			req.RequestID = "conflict"
			req.Headers = http.Header{"X-Session": {"different"}}
			if out := e.Before(req); !out.Terminate {
				t.Fatal("conflicting continuation downgraded")
			}
			e.Complete("conflict")
		})
	}
}

func TestConcurrentSessionRequestsReuseOneMarker(t *testing.T) {
	e := configured(t, scopedConfig+privateRule)
	var wg sync.WaitGroup
	out := make(chan string, 40)
	for i := 0; i < 40; i++ {
		wg.Go(func() {
			id := fmt.Sprintf("request-%d", i)
			req := pluginapi.RequestInterceptRequest{RequestID: id, SourceFormat: "openai-response", Headers: http.Header{"X-Session": {"same"}}, Metadata: map[string]any{"caller_scope": "caller"}, Body: []byte(`{"input":"PRIVATE"}`)}
			r := e.Before(req)
			if r.Terminate {
				t.Error(string(r.ResponseBody))
				return
			}
			out <- string(r.Body)
			e.Complete(id)
		})
	}
	wg.Wait()
	close(out)
	expected := ""
	for body := range out {
		if expected == "" {
			expected = body
		}
		if body != expected {
			t.Fatal("same session allocated different markers")
		}
	}
	if stats := e.mappings.Stats(); stats.Sessions != 1 || stats.Entries != 1 || stats.Leases != 0 {
		t.Fatal(stats)
	}
}

func TestSessionFailureDoesNotDeleteAnotherRequestsMapping(t *testing.T) {
	e := configured(t, scopedConfig+privateRule)
	req := pluginapi.RequestInterceptRequest{RequestID: "one", SourceFormat: "openai-response", Headers: http.Header{"X-Session": {"same"}}, Metadata: map[string]any{"caller_scope": "caller"}, Body: []byte(`{"input":"PRIVATE"}`)}
	one := e.Before(req)
	if one.Terminate {
		t.Fatal(one)
	}
	e.Complete("one")
	if err := e.Configure([]byte("on_request_error: passthrough\nlimits: {request_state_bytes: 20}\n" + scopedConfig + privateRule)); err != nil {
		t.Fatal(err)
	}
	req.RequestID = "failed"
	out := e.Before(req)
	if out.Terminate || len(out.Body) > 0 {
		t.Fatal(out)
	}
	e.Complete("failed")
	if stats := e.mappings.Stats(); stats.Entries != 1 {
		t.Fatal("rollback removed a previously committed secret", stats)
	}
	if err := e.Configure([]byte(scopedConfig + privateRule)); err != nil {
		t.Fatal(err)
	}
	req.RequestID = "again"
	if out := e.Before(req); !bytes.Equal(out.Body, one.Body) {
		t.Fatal("mapping changed after unrelated rollback")
	}
}
