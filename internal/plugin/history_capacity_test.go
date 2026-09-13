package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/session"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHistoryCapacityDoesNotDegradeOtherCallers(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			e := configured(t, privateRule+scopedConfig)
			request := func(id, caller string) pluginapi.RequestInterceptRequest {
				return pluginapi.RequestInterceptRequest{RequestID: id, SourceFormat: "openai-response", Body: []byte(`{"input":"PRIVATE"}`),
					Headers: http.Header{"X-Session": {"same"}}, Metadata: map[string]any{"caller_scope": caller}}
			}
			// Fill the global index across callers; one caller must not consume
			// more than its share once per-caller retention limits are enforced.
			for caller := 0; caller < session.MaxReferences/session.MaxCallerReferences; caller++ {
				id := fmt.Sprintf("seed-%d", caller)
				if out := e.Before(request(id, id)); out.Terminate {
					t.Fatal(string(out.ResponseBody))
				}
				refs := make([]protocol.Reference, session.MaxCallerReferences)
				for i := range refs {
					refs[i] = protocol.Reference{Kind: "response", ID: fmt.Sprintf("%d-%04d", caller, i)}
				}
				if err := e.sessions.Record(e.state(id).selection, refs); err != nil {
					t.Fatal(err)
				}
				e.Complete(id)
			}
			if e.sessions.Stats().References != session.MaxReferences {
				t.Fatal("global capacity fixture was not filled")
			}
			req := request("fresh", "other-caller")
			masked := e.Before(req)
			if masked.Terminate {
				t.Fatal(string(masked.ResponseBody))
			}
			var body struct{ Input string }
			if err := json.Unmarshal(masked.Body, &body); err != nil {
				t.Fatal(err)
			}
			response := []byte(`{"id":"fresh-response","output_text":"` + body.Input + `","output":[]}`)
			if streaming {
				created := []byte(`{"type":"response.created","response":{"id":"fresh-response","output":[]}}`)
				first := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: req.RequestID, SourceFormat: req.SourceFormat, Body: created})
				if len(first.Body) == 0 || first.DropChunk {
					t.Fatal("index capacity stopped stream restoration")
				}
				for _, fragment := range []string{body.Input[:30], body.Input[30:]} {
					raw := []byte(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"` + fragment + `"}`)
					out := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: req.RequestID, SourceFormat: req.SourceFormat, Body: raw})
					if len(out.Body) == 0 || out.DropChunk || (fragment == body.Input[30:] && !bytes.Contains(out.Body, []byte("PRIVATE"))) {
						t.Fatalf("stream input was not restored: %s", out.Body)
					}
				}
				out := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: req.RequestID, SourceFormat: req.SourceFormat,
					Body: append(append([]byte(`{"type":"response.completed","response":`), response...), '}')})
				if !bytes.Contains(out.Body, []byte("PRIVATE")) {
					t.Fatalf("stream completion was not restored: %s", out.Body)
				}
			} else {
				out := e.Response(pluginapi.ResponseInterceptRequest{RequestID: req.RequestID, SourceFormat: req.SourceFormat, Body: response})
				if !bytes.Contains(out.Body, []byte("PRIVATE")) {
					t.Fatalf("other caller restoration degraded: body=%q stats=%+v", out.Body, e.Stats())
				}
			}
			e.Complete(req.RequestID)
			req.RequestID = "continued"
			req.Headers = nil
			req.Body = []byte(`{"input":"PRIVATE","previous_response_id":"fresh-response"}`)
			next := e.Before(req)
			if next.Terminate || !bytes.Contains(next.Body, []byte(body.Input)) {
				t.Fatalf("new response reference was not retained: %s %s", next.Body, next.ResponseBody)
			}
			e.Complete(req.RequestID)
			if stats := e.Stats(); stats.ResponseDegraded != 0 || stats.HistoryCapacitySkipped != 0 {
				t.Fatal(e.Stats())
			}
		})
	}
}

func TestUnrecordableHistoryDoesNotStopCurrentRestoration(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			config := strings.Replace(scopedConfig, "session:\n", fmt.Sprintf("session:\n  max_id_bytes: %d\n", session.MaxCallerReferenceBytes), 1)
			// Isolate the history byte budget from the independent event limit.
			e := configured(t, privateRule+config+"limits: {event_bytes: 2097152}\n")
			req := pluginapi.RequestInterceptRequest{RequestID: "first", SourceFormat: "openai-response", Body: []byte(`{"input":"PRIVATE"}`),
				Headers: http.Header{"X-Session": {"same"}}, Metadata: map[string]any{"caller_scope": "caller"}}
			masked := e.Before(req)
			var input struct{ Input string }
			if masked.Terminate || json.Unmarshal(masked.Body, &input) != nil {
				t.Fatalf("request failed: %s", masked.ResponseBody)
			}
			// Valid under max_id_bytes but too large for the history index
			// once its bookkeeping is included in the caller byte budget.
			id := strings.Repeat("r", session.MaxCallerReferenceBytes)
			response := map[string]any{"id": id, "output_text": input.Input, "output": []any{}}
			var restored []byte
			if streaming {
				raw, _ := json.Marshal(map[string]any{"type": "response.created", "response": map[string]any{"id": id, "output": []any{}}})
				first := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: req.RequestID, SourceFormat: req.SourceFormat, Body: raw})
				if len(first.Body) == 0 || first.DropChunk {
					t.Fatal("unrecordable history ID stopped stream")
				}
				raw, _ = json.Marshal(map[string]any{"type": "response.completed", "response": response})
				restored = e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: req.RequestID, SourceFormat: req.SourceFormat, Body: raw}).Body
			} else {
				raw, _ := json.Marshal(response)
				restored = e.Response(pluginapi.ResponseInterceptRequest{RequestID: req.RequestID, SourceFormat: req.SourceFormat, Body: raw}).Body
			}
			if !bytes.Contains(restored, []byte("PRIVATE")) || !bytes.Contains(restored, []byte(id)) {
				t.Fatal("index capacity lost current restoration or changed response ID")
			}
			skipped := uint64(1)
			if streaming {
				skipped = 2
			}
			if stats := e.Stats(); stats.ResponseDegraded != 0 || stats.HistoryCapacitySkipped != skipped || e.sessions.Stats().References != 0 {
				t.Fatalf("capacity skip accounting: %+v", stats)
			}
			e.Complete(req.RequestID)
			req.RequestID = "unrecorded-history"
			// An explicit session must not let an unknown reference bypass
			// history validation after a capacity skip.
			req.Body, _ = json.Marshal(map[string]any{"input": "PRIVATE", "previous_response_id": id})
			if out := e.Before(req); !out.Terminate || !bytes.Contains(out.ResponseBody, []byte("session_resolution_failed")) {
				t.Fatal("unrecorded history reference was accepted")
			}
			e.Complete(req.RequestID)
		})
	}
}

func TestInvalidHistoryRegistrationStillDegrades(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, responseID := range []string{" invalid ", "conflicting"} {
			t.Run(fmt.Sprintf("stream=%v/id=%s", streaming, responseID), func(t *testing.T) {
				e := configured(t, privateRule+scopedConfig)
				req := pluginapi.RequestInterceptRequest{RequestID: "first", SourceFormat: "openai-response", Body: []byte(`{"input":"PRIVATE"}`),
					Headers: http.Header{"X-Session": {"first"}}, Metadata: map[string]any{"caller_scope": "caller"}}
				if out := e.Before(req); out.Terminate {
					t.Fatal(string(out.ResponseBody))
				}
				if err := e.sessions.Record(e.state(req.RequestID).selection, []protocol.Reference{{Kind: "response", ID: "conflicting"}}); err != nil {
					t.Fatal(err)
				}
				e.Complete(req.RequestID)
				req.RequestID = "second"
				req.Headers.Set("X-Session", "second")
				masked := e.Before(req)
				var input struct{ Input string }
				if masked.Terminate || json.Unmarshal(masked.Body, &input) != nil {
					t.Fatal("second request failed")
				}
				response := map[string]any{"id": responseID, "output_text": input.Input, "output": []any{}}
				var restored []byte
				if streaming {
					raw, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
					restored = e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: req.RequestID, SourceFormat: req.SourceFormat, Body: raw}).Body
					if !e.state(req.RequestID).streamStopped || e.state(req.RequestID).stream != nil {
						t.Fatal("invalid reference left stream restoration active")
					}
				} else {
					raw, _ := json.Marshal(response)
					restored = e.Response(pluginapi.ResponseInterceptRequest{RequestID: req.RequestID, SourceFormat: req.SourceFormat, Body: raw}).Body
				}
				if len(restored) != 0 || e.Stats().ResponseDegraded != 1 || e.Stats().HistoryCapacitySkipped != 0 {
					t.Fatalf("invalid reference bypassed validation: %+v", e.Stats())
				}
				e.Complete(req.RequestID)
			})
		}
	}
}
