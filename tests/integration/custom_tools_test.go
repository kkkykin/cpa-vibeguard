//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
