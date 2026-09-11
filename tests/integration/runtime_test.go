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
	"sync/atomic"
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

func runtimeResponses(t *testing.T, h *hostProcess, input, previous, session string) (int, []byte) {
	t.Helper()
	req := map[string]any{"model": "fixture-ws", "input": input}
	if previous != "" {
		req["previous_response_id"] = previous
	}
	body, _ := json.Marshal(req)
	headers := make(http.Header)
	if session != "" {
		headers.Set("X-Session-ID", session)
	}
	return requestHTTP(t, "POST", h.url+"/v1/responses", body, headers)
}

func TestHostRestartAndBinaryReplacement(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			captured := make(chan string, 16)
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				marker := redactedUpstream(t, "codex", raw)
				captured <- marker
				id := fmt.Sprintf("resp_runtime_%d", calls.Add(1))
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range responseEvents(marker, id) {
					writeSSE(w, event)
				}
			}))
			defer upstream.Close()
			pluginDir := t.TempDir()
			copyPlugin(t, pluginDir, "cpa-vibeguard-v0.1.0.so")
			cfg := hostRule + runtimeSession + "\n      priority: 0"
			h := startHostConfigured(t, hostSettings{Providers: websocketProviders(upstream.URL, false), PluginConfig: cfg, PluginDir: pluginDir})
			status, raw := runtimeResponses(t, h, "PRIVATE", "", "runtime")
			if status != 200 || !bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte(token.Prefix)) {
				t.Fatalf("initial response: %d %s", status, raw)
			}
			oldMarker := <-captured
			var response map[string]any
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatal(err)
			}
			previous := response["id"].(string)
			// With no explicit source, a registered response ID recovers the
			// original authenticated scope before its instance is replaced.
			status, raw = runtimeResponses(t, h, "PRIVATE", previous, "")
			if status != 200 || <-captured != oldMarker {
				t.Fatalf("live history binding: %d %s", status, raw)
			}
			if replacement {
				// All HTTP responses are fully consumed and no model request is
				// in flight. Install to a new versioned path, never over mmap'd
				// bytes. First exercise rollback of a failed replacement.
				atomicFile(t, filepath.Join(pluginDir, "cpa-vibeguard-v0.1.1.so"), []byte("invalid fixture library"))
				offset := h.changeConfig(t, "priority: 0", "priority: 1")
				h.awaitLog(t, offset, "failed to load plugin")
				status, raw = runtimeResponses(t, h, "PRIVATE", previous, "runtime")
				if status != 200 || <-captured != oldMarker {
					t.Fatalf("failed replacement did not reactivate the old instance: %d %s", status, raw)
				}
				copyPlugin(t, pluginDir, "cpa-vibeguard-v0.1.2.so")
				offset = h.changeConfig(t, "priority: 1", "priority: 2")
				h.awaitLog(t, offset, "plugin hot reloaded")
			} else {
				h.stop(t)
				h.start(t)
			}
			before := calls.Load()
			status, raw = runtimeResponses(t, h, "PRIVATE", previous, "runtime")
			if status != 400 || !bytes.Contains(raw, []byte("session_resolution_failed")) || calls.Load() != before {
				t.Fatalf("stale history was not rejected: %d %s", status, raw)
			}
			status, raw = runtimeResponses(t, h, "PRIVATE", "", "runtime")
			if status != 200 || !bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte(token.Prefix)) {
				t.Fatalf("new instance roundtrip: %d %s", status, raw)
			}
			if marker := <-captured; marker == oldMarker {
				t.Fatal("fresh instance reused the discarded secret mapping")
			}
			status, raw = runtimeResponses(t, h, oldMarker, "", "runtime")
			if status != 200 || !bytes.Contains(raw, []byte(oldMarker)) || bytes.Contains(raw, []byte("PRIVATE")) || <-captured != oldMarker {
				t.Fatalf("new instance recovered an unauthorized old marker: %d %s", status, raw)
			}
		})
	}
}
