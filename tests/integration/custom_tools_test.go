//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

func TestHostCustomToolDescriptionRoundTrip(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Messages []struct{ Content string }
			Tools    []struct {
				Type   string
				Custom struct {
					Name, Description string
					Format            struct{ Type string }
				}
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(body.Messages) != 1 || len(body.Tools) != 1 {
			t.Error("unexpected request shape")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		tool := body.Tools[0]
		if !token.Valid(tool.Custom.Description) || tool.Custom.Description != body.Messages[0].Content || tool.Type != "custom" || tool.Custom.Name != "PRIVATE_TOOL" || tool.Custom.Format.Type != "text" {
			t.Errorf("unprotected or altered custom tool: %+v", tool)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
					"id": "call-audit", "type": "custom", "custom": map[string]any{"name": tool.Custom.Name, "input": tool.Custom.Description},
				}},
			},
		}}})
	}))
	defer upstream.Close()
	h := startHost(t, upstream.URL, hostRule)
	status, raw := post(t, h.url+"/v1/chat/completions", `{"model":"fixture-chat","messages":[{"role":"user","content":"PRIVATE"}],"tools":[{"type":"custom","custom":{"name":"PRIVATE_TOOL","description":"PRIVATE","format":{"type":"text"}}}]}`, false)
	var response struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct{ Custom struct{ Name, Input string } } `json:"tool_calls"`
			}
		}
	}
	if status != http.StatusOK || calls.Load() != 1 || json.Unmarshal(raw, &response) != nil || len(response.Choices) != 1 || len(response.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("custom tool response: HTTP %d, %s", status, raw)
	}
	tool := response.Choices[0].Message.ToolCalls[0].Custom
	if tool.Input != "PRIVATE" || tool.Name != "PRIVATE_TOOL" {
		t.Fatalf("custom tool response not restored: %s", raw)
	}
}

func TestHostCustomToolStreamsEverySplit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Content string }
			Metadata struct {
				Split   int
				Initial string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) != 1 {
			t.Errorf("invalid upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		marker := req.Messages[0].Content
		if !token.Valid(marker) {
			t.Error("message was not redacted")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(call map[string]any) {
			writeSSE(w, map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"tool_calls": []any{call}}, "finish_reason": nil,
			}}})
		}
		first := map[string]any{"index": 0, "id": "call-custom", "type": "custom"}
		if req.Metadata.Initial != "type-only" {
			input := ""
			if req.Metadata.Initial == "input" {
				input = marker[:req.Metadata.Split]
			}
			first["custom"] = map[string]any{"name": "run", "input": input}
		}
		send(first)
		if req.Metadata.Initial != "input" {
			send(map[string]any{"index": 0, "custom": map[string]any{"name": "run", "input": marker[:req.Metadata.Split]}})
		}
		send(map[string]any{"index": 0, "custom": map[string]any{"input": marker[req.Metadata.Split:]}})
		writeSSE(w, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := startHost(t, upstream.URL, hostRule)
	for _, initial := range []string{"input", "empty-input", "type-only"} {
		for split := 0; split <= token.Length; split++ {
			t.Run(fmt.Sprintf("%s/%d", initial, split), func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"fixture-chat","messages":[{"role":"user","content":"PRIVATE"}],"stream":true,"tools":[{"type":"custom","custom":{"name":"run","format":{"type":"text"}}}],"metadata":{"split":%d,"initial":%q}}`, split, initial)
				status, raw := post(t, h.url+"/v1/chat/completions", body, false)
				if status != http.StatusOK {
					t.Fatalf("HTTP %d: %s", status, raw)
				}
				var input strings.Builder
				seenID, seenName, finished := false, false, false
				for _, event := range streamObjects(t, raw) {
					choices, _ := event["choices"].([]any)
					for _, v := range choices {
						choice := v.(map[string]any)
						finished = finished || choice["finish_reason"] == "tool_calls"
						delta, _ := choice["delta"].(map[string]any)
						calls, _ := delta["tool_calls"].([]any)
						for _, v := range calls {
							call := v.(map[string]any)
							custom, _ := call["custom"].(map[string]any)
							text, _ := custom["input"].(string)
							input.WriteString(text)
							seenID = seenID || call["id"] == "call-custom"
							seenName = seenName || custom["name"] == "run"
						}
					}
				}
				if input.String() != "PRIVATE" || !seenID || !seenName || !finished {
					t.Fatalf("custom tool stream corrupted: %q, id=%v name=%v finish=%v, %s", input.String(), seenID, seenName, finished, raw)
				}
			})
		}
	}
}
