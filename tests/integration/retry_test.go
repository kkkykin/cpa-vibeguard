//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

func TestHostCredentialRetryKeepsProtectedBody(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			type attempt struct {
				body []byte
				auth string
			}
			captured := make(chan attempt, 4)
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				marker := redactedUpstream(t, "openai", raw)
				captured <- attempt{raw, r.Header.Get("Authorization")}
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(503)
					fmt.Fprint(w, `{"error":{"message":"fixture retry","type":"server_error"}}`)
					return
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					writeSSE(w, map[string]any{"id": "retry", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": marker}, "finish_reason": nil}}})
					writeSSE(w, map[string]any{"id": "retry", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
					fmt.Fprint(w, "data: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"id": "retry", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": marker}, "finish_reason": "stop"}}})
			}))
			defer upstream.Close()
			providers := fmt.Sprintf("openai-compatibility:\n  - name: retry\n    base-url: %q\n    api-key-entries: [{api-key: fixture-a}, {api-key: fixture-b}]\n    models: [{name: fixture-chat}]\n", upstream.URL+"/v1")
			h, probe := startProbedHost(t, hostSettings{Providers: providers, PluginConfig: hostRule + "\n      preserve_marker_hint: true", RequestRetry: 1}, false)
			start := probe.mark(t)
			status, raw := post(t, h.url+"/v1/chat/completions", fmt.Sprintf(`{"model":"fixture-chat","messages":[{"role":"user","content":"PRIVATE"}],"stream":%v}`, stream), false)
			if status != 200 || !bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte(token.Prefix)) || calls.Load() != 2 {
				t.Fatalf("retried response: %d %s, attempts=%d", status, raw, calls.Load())
			}
			first, second := <-captured, <-captured
			if first.auth == second.auth || !bytes.Equal(first.body, second.body) || bytes.Count(second.body, []byte("Preserve privacy markers exactly")) != 1 {
				t.Fatal("credential retry changed protected input or duplicated the hint")
			}
			before, after := 0, 0
			for _, hook := range probe.completed(t, start) {
				if hook.Method == "request.intercept_before" {
					before++
				}
				if hook.Method == "request.intercept_after" {
					after++
				}
			}
			if before != 1 || after != 2 {
				t.Fatalf("retry lifecycle: before=%d after=%d", before, after)
			}
		})
	}
}

func TestHostConcurrentSessionMarkers(t *testing.T) {
	const count = 12
	type capture struct{ group, marker string }
	captured := make(chan capture, count)
	release := make(chan struct{})
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Messages []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 1 {
			t.Error("invalid concurrent fixture input")
			w.WriteHeader(400)
			return
		}
		content := request.Messages[0].Content
		if len(content) != token.Length+2 || !token.Valid(content[2:]) {
			t.Error("concurrent request reached upstream without a marker")
			w.WriteHeader(400)
			return
		}
		captured <- capture{content[:1], content[2:]}
		if calls.Add(1) == count {
			close(release)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "concurrent", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
	}))
	defer upstream.Close()
	h := startHost(t, upstream.URL, hostRule+runtimeSession)
	type result struct {
		status int
		body   []byte
		err    error
	}
	results := make(chan result, count)
	client := http.Client{Timeout: 10 * time.Second}
	for i := 0; i < count; i++ {
		go func(group string) {
			body := fmt.Sprintf(`{"model":"fixture-chat","messages":[{"role":"user","content":"%s PRIVATE"}]}`, group)
			req, err := http.NewRequest("POST", h.url+"/v1/chat/completions", strings.NewReader(body))
			if err != nil {
				results <- result{err: err}
				return
			}
			req.Header = http.Header{"Authorization": {"Bearer fixture-client"}, "Content-Type": {"application/json"}, "X-Session-ID": {group}}
			res, err := client.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			raw, err := io.ReadAll(res.Body)
			res.Body.Close()
			results <- result{res.StatusCode, raw, err}
		}(string(rune('A' + i%2)))
	}
	for i := 0; i < count; i++ {
		r := <-results
		if r.err != nil || r.status != 200 || !bytes.Contains(r.body, []byte("PRIVATE")) || bytes.Contains(r.body, []byte(token.Prefix)) {
			t.Fatalf("concurrent response: %v %d %s", r.err, r.status, r.body)
		}
	}
	markers := make(map[string]string)
	for i := 0; i < count; i++ {
		c := <-captured
		if marker := markers[c.group]; marker != "" && marker != c.marker {
			t.Fatal("concurrent same-session requests assigned different markers")
		}
		markers[c.group] = c.marker
	}
	if len(markers) != 2 || markers["A"] == markers["B"] {
		t.Fatal("concurrent sessions shared a marker")
	}
}
