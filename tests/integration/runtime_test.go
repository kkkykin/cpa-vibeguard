//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

const runtimeSession = "\n      session:\n        profiles: [{namespace: runtime, sources: [{header: X-Session-ID}]}]"

func (h *hostProcess) changeConfig(t *testing.T, before, after string) int {
	t.Helper()
	h.awaitLog(t, 0, "file watcher started")
	raw, err := os.ReadFile(h.configPath)
	if err != nil || bytes.Count(raw, []byte(before)) != 1 {
		t.Fatalf("configuration replacement: %v", err)
	}
	log, err := os.ReadFile(h.logPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := bytes.Replace(raw, []byte(before), []byte(after), 1)
	// The pinned CPA watches the config inode, not its parent directory.
	// Binary installation below uses rename; config reload needs a write here.
	if err := os.WriteFile(h.configPath, updated, 0600); err != nil {
		t.Fatal(err)
	}
	return len(log)
}

func atomicFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path+".next", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".next", path); err != nil {
		t.Fatal(err)
	}
}

func (h *hostProcess) awaitLog(t *testing.T, offset int, needle string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(h.logPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) >= offset && bytes.Contains(raw[offset:], []byte(needle)) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("host did not report %q", needle)
}

func runtimeChat(t *testing.T, h *hostProcess, text string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": "fixture-chat", "metadata": map[string]any{"session_id": "runtime"}, "messages": []any{map[string]any{"role": "user", "content": text}}})
	return requestHTTP(t, "POST", h.url+"/v1/chat/completions", body, http.Header{"X-Session-ID": {"runtime"}})
}

func TestHostConfigurationSnapshots(t *testing.T) {
	captured := make(chan string, 32)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Content string }
			Stream   bool
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) != 1 {
			t.Error("invalid snapshot fixture input")
			w.WriteHeader(400)
			return
		}
		text := req.Messages[0].Content
		captured <- text
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": "snapshot", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}}})
			return
		}
		if !token.Valid(text) {
			t.Error("held request was not redacted")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, map[string]any{"id": "snapshot", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "sent " + text[:20]}, "finish_reason": nil}}})
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		writeSSE(w, map[string]any{"id": "snapshot", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text[20:]}, "finish_reason": nil}}})
		writeSSE(w, map[string]any{"id": "snapshot", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	baseConfig := hostRule + runtimeSession
	h := startHost(t, upstream.URL, baseConfig)
	req, _ := http.NewRequest("POST", h.url+"/v1/chat/completions", strings.NewReader(`{"model":"fixture-chat","messages":[{"role":"user","content":"PRIVATE"}],"stream":true}`))
	req.Header = http.Header{"Authorization": {"Bearer fixture-client"}, "Content-Type": {"application/json"}, "X-Session-ID": {"runtime"}}
	client := http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	first, err := reader.ReadBytes('\n')
	if err != nil || res.StatusCode != 200 || !bytes.Contains(first, []byte("sent ")) || bytes.Contains(first, []byte(token.Prefix)) {
		t.Fatalf("held stream: %v %d %s", err, res.StatusCode, first)
	}
	marker := <-captured
	badConfig := strings.Replace(baseConfig, "regex: PRIVATE", "regex: '('", 1)
	offset := h.changeConfig(t, baseConfig, badConfig)
	h.awaitLog(t, offset, "reconfigure_rejected")
	status, raw := runtimeChat(t, h, "PRIVATE")
	if status != 200 || bytes.Contains(raw, []byte(token.Prefix)) || <-captured != marker {
		t.Fatalf("failed update did not retain protected configuration: %d %s", status, raw)
	}
	// The new tiny event limit must not apply to the existing stream. Disabling
	// the rule only affects future scans, not restoration of its old mappings.
	newConfig := baseConfig + "\n      disabled_rules: [fixture]\n      limits: {event_bytes: 64}"
	h.changeConfig(t, badConfig, newConfig)
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, raw = runtimeChat(t, h, "PRIVATE "+marker)
		text := <-captured
		if status != 200 || !bytes.Contains(raw, []byte("PRIVATE PRIVATE")) || bytes.Contains(raw, []byte(token.Prefix)) {
			t.Fatalf("old mapping restoration after update: %d %s", status, raw)
		}
		if text == "PRIVATE "+marker {
			break
		}
		if text != marker+" "+marker || time.Now().After(deadline) {
			t.Fatalf("rule disable did not take effect: %q", text)
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	for _, event := range streamObjects(t, append(first, rest...)) {
		for _, choice := range event["choices"].([]any) {
			delta := choice.(map[string]any)["delta"].(map[string]any)
			text, _ := delta["content"].(string)
			content.WriteString(text)
		}
	}
	if content.String() != "sent PRIVATE" {
		t.Fatalf("active request lost its snapshot: %q", content.String())
	}
	// Re-enabling a rule reuses the compatible session mapping.
	h.changeConfig(t, newConfig, baseConfig)
	deadline = time.Now().Add(10 * time.Second)
	for {
		status, _ = runtimeChat(t, h, "PRIVATE")
		text := <-captured
		if status == 200 && text == marker {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("re-enabled rule did not preserve the session mapping")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A source change with the same namespace, caller and exact ID is still
	// the same session; the source's type/index is not part of the scope key.
	jsonConfig := strings.Replace(baseConfig, "header: X-Session-ID", "json: /metadata/session_id", 1)
	h.changeConfig(t, baseConfig, jsonConfig)
	deadline = time.Now().Add(10 * time.Second)
	for {
		status, raw = requestHTTP(t, "POST", h.url+"/v1/chat/completions", []byte(`{"model":"fixture-chat","metadata":{"session_id":"runtime"},"messages":[{"role":"user","content":"PRIVATE"}]}`), nil)
		text := <-captured
		if status != 200 || !bytes.Contains(raw, []byte("PRIVATE")) {
			t.Fatalf("source reconfiguration response: %d %s", status, raw)
		}
		if text == marker {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session source reconfiguration discarded compatible mappings")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Keep path handling local to these fixtures; neither the reference checkout
// nor the workspace's built library is overwritten during host replacement.
func copyPlugin(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../..", "dist", "cpa-vibeguard.so"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	atomicFile(t, path, raw)
	return path
}
