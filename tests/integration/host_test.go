//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

type hostProcess struct {
	url     string
	cmd     *exec.Cmd
	done    chan error
	logPath string
}

func startHost(t *testing.T, upstream, pluginConfig string) *hostProcess {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "bin/cpa-host")
	if _, err := os.Stat(binary); err != nil {
		t.Fatal("build pinned host first: bash scripts/build-host.sh")
	}
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`host: "127.0.0.1"
port: %d
auth-dir: %q
api-keys: [fixture-client]
commercial-mode: true
disable-cooling: true
request-retry: 0
remote-management:
  disable-control-panel: true
plugins:
  enabled: true
  dir: %q
  configs:
    cpa-vibeguard:
      enabled: true
%s
openai-compatibility:
  - name: fixture
    base-url: %q
    api-key-entries:
      - api-key: fixture-upstream
    models:
      - name: fixture-chat
        alias: fixture-chat
`, port, filepath.Join(dir, "auths"), filepath.Join(root, "dist"), pluginConfig, upstream+"/v1")
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "host.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config", configPath, "--local-model")
	cmd.Dir = dir
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	h := &hostProcess{url: fmt.Sprintf("http://127.0.0.1:%d", port), cmd: cmd, done: make(chan error, 1), logPath: logPath}
	go func() { h.done <- cmd.Wait(); log.Close() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-h.done
		}
		if t.Failed() {
			b, _ := os.ReadFile(logPath)
			t.Logf("host log:\n%s", b)
		}
	})
	deadline := time.After(20 * time.Second)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-h.done:
			h.done <- err
			t.Fatalf("host exited at startup: %v", err)
		case <-deadline:
			t.Fatal("host startup timed out")
		case <-ticker.C:
			resp, err := http.Get(h.url + "/healthz")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return h
				}
			}
		}
	}
}

func post(t *testing.T, url, body string, skip bool) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer fixture-client")
	if skip {
		req.Header.Set("X-Vibeguard-Skip", "true")
	}
	client := http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func TestHostLifecycle(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		var obj map[string]any
		if json.Unmarshal(body, &obj) != nil {
			t.Error("invalid upstream JSON")
		}
		if obj["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fixture reply\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"fixture reply"},"finish_reason":"stop"}]}`)
		}
	}))
	defer upstream.Close()
	h := startHost(t, upstream.URL, "      bypass: {headers: [{name: X-Vibeguard-Skip, value: 'true'}]}\n      limits: {max_inflight: 1}")
	for _, stream := range []bool{false, true, false} {
		body := fmt.Sprintf(`{"model":"fixture-chat","messages":[{"role":"user","content":"fixture input"}],"stream":%v}`, stream)
		var status int
		var raw []byte
		for i := 0; i < 20; i++ {
			status, raw = post(t, h.url+"/v1/chat/completions", body, true)
			if status != 429 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if status != 200 || !bytes.Contains(raw, []byte("fixture reply")) {
			t.Fatalf("bypass roundtrip: %d %s", status, raw)
		}
	}
	before := upstreamCalls.Load()
	status, raw := post(t, h.url+"/v1/chat/completions", `{"model":"fixture-chat","messages":[{"role":"user","content":{"unsupported":true}}]}`, false)
	if status != 400 || !bytes.Contains(raw, []byte("vibeguard_request_rejected")) {
		t.Fatalf("business rejection: %d %s", status, raw)
	}
	if upstreamCalls.Load() != before {
		t.Fatal("rejected request reached upstream")
	}
	logs, _ := os.ReadFile(h.logPath)
	if !bytes.Contains(logs, []byte("plugin registered")) || !bytes.Contains(logs, []byte("cpa-vibeguard")) {
		t.Fatalf("missing real host registration evidence: %s", logs)
	}
}

func TestHostProtectedChatAndLegacy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Messages []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if len(request.Messages) != 1 || !token.Valid(request.Messages[0].Content) {
			t.Errorf("upstream did not receive a redacted message: %+v", request)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": request.Messages[0].Content}, "finish_reason": "stop"}}})
	}))
	defer upstream.Close()
	h := startHost(t, upstream.URL, "      rule_sources: [{inline: {extend: {useDefault: false}, rules: [{id: fixture, regex: PRIVATE}]}}]")
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"fixture-chat","messages":[{"role":"user","content":"PRIVATE"}]}`},
		{"/v1/completions", `{"model":"fixture-chat","prompt":"PRIVATE"}`},
	} {
		status, raw := post(t, h.url+tc.path, tc.body, false)
		if status != 200 || !bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte(token.Prefix)) {
			t.Fatalf("%s: %d %s", tc.path, status, raw)
		}
	}
}

func TestHostSessionHistoricalPrefix(t *testing.T) {
	var mu sync.Mutex
	var histories [][]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Messages []json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		histories = append(histories, request.Messages)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	h := startHost(t, upstream.URL, "      rule_sources: [{inline: {extend: {useDefault: false}, rules: [{id: fixture, regex: PRIVATE}]}}]\n      session:\n        profiles:\n          - namespace: fixture\n            sources: [{json: /metadata/session_id}]")
	for _, body := range []string{
		`{"model":"fixture-chat","metadata":{"session_id":"same"},"messages":[{"role":"user","content":"PRIVATE"}]}`,
		`{"model":"fixture-chat","metadata":{"session_id":"same"},"messages":[{"role":"user","content":"PRIVATE"},{"role":"user","content":"PRIVATE again"}]}`,
	} {
		if status, raw := post(t, h.url+"/v1/chat/completions", body, false); status != 200 {
			t.Fatalf("session request: %d %s", status, raw)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(histories) != 2 || !bytes.Equal(histories[0][0], histories[1][0]) || bytes.Contains(histories[0][0], []byte("PRIVATE")) {
		t.Fatalf("redacted history prefix not stable: %s", histories)
	}
}
