//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/testutil"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

func TestHostEmbeddedBase64Images(t *testing.T) {
	_, png := testutil.PNGWithPhone(t)
	const phone = "13800138000"
	markdown := " ![image](data:image/png;base64," + png + ")"
	type arguments struct {
		Image   string `json:"image_base64"`
		Caption string `json:"caption"`
	}
	type call struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	type message struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		ToolCalls []call `json:"tool_calls,omitempty"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []message
			Stream   bool
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 3 || len(request.Messages[1].ToolCalls) != 1 {
			t.Errorf("invalid media fixture request: %v", err)
			w.WriteHeader(400)
			return
		}
		marker := strings.SplitN(request.Messages[0].Content, " ", 2)[0]
		if !token.Valid(marker) || request.Messages[0].Content != marker+markdown {
			t.Error("Markdown image changed or adjacent text escaped redaction")
		}
		tool := request.Messages[1].ToolCalls[0]
		var args arguments
		if json.Unmarshal([]byte(tool.Function.Arguments), &args) != nil || args.Image != png || args.Caption != marker {
			t.Error("tool argument image changed or caption escaped redaction")
		}
		var result struct {
			Content []struct{ Type, Data, Text string }
		}
		if json.Unmarshal([]byte(request.Messages[2].Content), &result) != nil || len(result.Content) != 2 || result.Content[0].Data != png || result.Content[1].Text != marker {
			t.Error("string-encoded tool result image changed or text escaped redaction")
		}
		response := message{Role: "assistant", Content: marker + markdown, ToolCalls: []call{tool}}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(w, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": response}}})
			writeSSE(w, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})
			fmt.Fprint(w, "data: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": response, "finish_reason": "tool_calls"}}})
		}
	}))
	defer upstream.Close()
	h := startHost(t, upstream.URL, "      rule_sources: [{inline: {rules: [{id: phone, regex: '\\b1[3-9][0-9]{9}\\b'}]}}]")
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			args, _ := json.Marshal(arguments{Image: png, Caption: phone})
			result, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "image", "data": png, "mimeType": "image/png"}, map[string]any{"type": "text", "text": phone}}})
			body, _ := json.Marshal(map[string]any{"model": "fixture-chat", "stream": stream, "messages": []any{
				map[string]any{"role": "user", "content": phone + markdown},
				map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "capture", "type": "function", "function": map[string]any{"name": "capture", "arguments": string(args)}}}},
				map[string]any{"role": "tool", "tool_call_id": "capture", "content": string(result)},
			}})
			status, raw := post(t, h.url+"/v1/chat/completions", string(body), false)
			if status != 200 {
				t.Fatalf("HTTP %d: %s", status, raw)
			}
			var messages []message
			if stream {
				for _, event := range streamObjects(t, raw) {
					encoded, _ := json.Marshal(event)
					var parsed struct{ Choices []struct{ Delta message } }
					if err := json.Unmarshal(encoded, &parsed); err != nil {
						t.Fatal(err)
					}
					for _, choice := range parsed.Choices {
						messages = append(messages, choice.Delta)
					}
				}
			} else {
				var parsed struct{ Choices []struct{ Message message } }
				if err := json.Unmarshal(raw, &parsed); err != nil {
					t.Fatal(err)
				}
				for _, choice := range parsed.Choices {
					messages = append(messages, choice.Message)
				}
			}
			var text, toolJSON strings.Builder
			for _, msg := range messages {
				text.WriteString(msg.Content)
				for _, tool := range msg.ToolCalls {
					toolJSON.WriteString(tool.Function.Arguments)
				}
			}
			var restored arguments
			if text.String() != phone+markdown || json.Unmarshal([]byte(toolJSON.String()), &restored) != nil || restored.Image != png || restored.Caption != phone {
				t.Fatalf("media round trip changed payload or lost text restoration: %s", raw)
			}
		})
	}
}
