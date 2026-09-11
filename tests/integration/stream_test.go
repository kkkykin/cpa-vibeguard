//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

const hostRule = "      rule_sources: [{inline: {extend: {useDefault: false}, rules: [{id: fixture, regex: PRIVATE}]}}]"

func writeSSE(w http.ResponseWriter, event any) {
	b, _ := json.Marshal(event)
	fmt.Fprintf(w, "data: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
func streamObjects(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		body := bytes.TrimSpace(line[5:])
		if len(body) == 0 || bytes.Equal(body, []byte("[DONE]")) {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("invalid client stream: %v %s", err, raw)
		}
		events = append(events, m)
	}
	return events
}
func TestHostProtectedChatStreamsEverySplit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Content string }
			Metadata struct {
				Split    int
				Abnormal bool
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if len(req.Messages) != 1 || !token.Valid(req.Messages[0].Content) {
			t.Error("upstream received plaintext")
			w.WriteHeader(400)
			return
		}
		marker := req.Messages[0].Content
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, map[string]any{"id": "stream", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "sent " + marker[:req.Metadata.Split]}, "finish_reason": nil}}})
		if req.Metadata.Abnormal {
			return
		}
		writeSSE(w, map[string]any{"id": "stream", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": marker[req.Metadata.Split:]}, "finish_reason": nil}, map[string]any{"index": 1, "delta": map[string]any{"content": marker}, "finish_reason": nil}}})
		writeSSE(w, map[string]any{"id": "stream", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}, map[string]any{"index": 1, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"total_tokens": 7}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := startHost(t, upstream.URL, hostRule)
	for _, path := range []string{"/v1/chat/completions", "/v1/completions"} {
		for split := 0; split <= token.Length; split++ {
			body := fmt.Sprintf(`{"model":"fixture-chat","messages":[{"role":"user","content":"PRIVATE"}],"stream":true,"metadata":{"split":%d}}`, split)
			if path == "/v1/completions" {
				body = fmt.Sprintf(`{"model":"fixture-chat","prompt":"PRIVATE","stream":true,"metadata":{"split":%d}}`, split)
			}
			status, raw := post(t, h.url+path, body, false)
			if status != 200 {
				t.Fatalf("%s split %d: %d %s", path, split, status, raw)
			}
			content := map[int]string{}
			for _, event := range streamObjects(t, raw) {
				choices, _ := event["choices"].([]any)
				for _, v := range choices {
					c := v.(map[string]any)
					i := int(c["index"].(float64))
					text, _ := c["text"].(string)
					if d, ok := c["delta"].(map[string]any); ok {
						text, _ = d["content"].(string)
					}
					content[i] += text
				}
			}
			if content[0] != "sent PRIVATE" || content[1] != "PRIVATE" {
				t.Fatalf("%s split %d: %v %s", path, split, content, raw)
			}
		}
	}
	for _, split := range []int{1, 13, 30, 47} {
		body := fmt.Sprintf(`{"model":"fixture-chat","messages":[{"role":"user","content":"PRIVATE"}],"stream":true,"metadata":{"split":%d,"abnormal":true}}`, split)
		status, raw := post(t, h.url+"/v1/chat/completions", body, false)
		if status != 200 || !bytes.Contains(raw, []byte("sent ")) || bytes.Contains(raw, []byte(token.Prefix)) || bytes.Contains(raw, []byte("PRIVATE")) {
			t.Fatalf("abnormal %d: %d %s", split, status, raw)
		}
	}
}

func TestHostStreamToolsAndNormalPartialTail(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		marker := req.Messages[0].Content
		if !token.Valid(marker) {
			t.Error("plaintext upstream")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		argument := `{"q":"` + marker + `"}`
		for i := 0; i < len(argument); i++ {
			writeSSE(w, map[string]any{"id": "tools", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "type": "function", "id": "tool_0", "function": map[string]any{"name": "lookup", "arguments": argument[i : i+1]}}}}, "finish_reason": nil}}})
		}
		writeSSE(w, map[string]any{"id": "tools", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "[[redacted:v1:"}, "finish_reason": nil}}})
		writeSSE(w, map[string]any{"id": "tools", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"total_tokens": 7}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := startHost(t, upstream.URL, hostRule)
	status, raw := post(t, h.url+"/v1/chat/completions", `{"model":"fixture-chat","messages":[{"role":"user","content":"PRIVATE"}],"stream":true}`, false)
	if status != 200 {
		t.Fatalf("%d %s", status, raw)
	}
	var arg, content strings.Builder
	usage := false
	for _, m := range streamObjects(t, raw) {
		if m["usage"] != nil {
			usage = true
		}
		for _, v := range m["choices"].([]any) {
			c := v.(map[string]any)
			d, _ := c["delta"].(map[string]any)
			if text, ok := d["content"].(string); ok {
				content.WriteString(text)
			}
			tools, _ := d["tool_calls"].([]any)
			for _, v := range tools {
				f := v.(map[string]any)["function"].(map[string]any)
				if text, ok := f["arguments"].(string); ok {
					arg.WriteString(text)
				}
			}
		}
	}
	var value map[string]string
	if json.Unmarshal([]byte(arg.String()), &value) != nil || value["q"] != "PRIVATE" || content.String() != "[[redacted:v1:" || !usage {
		t.Fatalf("tools/tail: %s %s usage=%v", arg.String(), content.String(), usage)
	}
}
