//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

func TestCaddyPrefixAndHeaderBoundary(t *testing.T) {
	received := make(chan string, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("prefix not stripped: %s", r.URL.Path)
		}
		var req struct {
			Messages []struct{ Content string }
			Stream   bool
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		text := req.Messages[0].Content
		received <- text
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(w, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": "stop"}}})
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": text}}}})
	}))
	defer upstream.Close()
	h := startHost(t, upstream.URL, hostRule+"\n      bypass: {headers: [{name: X-Vibeguard-Skip, value: 'true'}]}")
	root, _ := filepath.Abs("../..")
	binary := filepath.Join(root, "bin/caddy")
	if _, err := os.Stat(binary); err != nil {
		t.Fatal("build fixture proxy first: bash scripts/build-caddy.sh")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	example, err := os.ReadFile(filepath.Join(root, "examples/Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	config := "{\n admin off\n auto_https off\n}\n" + strings.ReplaceAll(strings.Replace(string(example), ":8080 {", "http://"+address+" {", 1), "127.0.0.1:8317", strings.TrimPrefix(h.url, "http://"))
	dir := t.TempDir()
	configPath := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "caddy.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "run", "--config", configPath, "--adapter", "caddyfile")
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); log.Close() }()
	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			b, _ := os.ReadFile(logPath)
			t.Logf("Caddy log: %s", b)
		}
	})
	url := "http://" + address
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := http.Get(url + "/healthz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("Caddy startup timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
	marker := ""
	for _, tc := range []struct {
		path           string
		stream, inject bool
		message        string
	}{
		{"/v1/chat/completions", false, true, "PRIVATE"},
		{"/unmasked/v1/chat/completions", false, false, "PRIVATE"},
		{"/v1/chat/completions", true, true, "PRIVATE"},
		{"/unmasked/v1/chat/completions", true, false, "known-marker"},
	} {
		message := tc.message
		if message == "known-marker" {
			message = marker
		}
		body, _ := json.Marshal(map[string]any{"model": "fixture-chat", "messages": []any{map[string]any{"role": "user", "content": message}}, "stream": tc.stream})
		headers := http.Header{}
		if tc.inject {
			headers.Set("X-Vibeguard-Skip", "true")
		}
		status, raw := requestHTTP(t, "POST", url+tc.path, body, headers)
		if status != 200 {
			t.Fatalf("%s: %d %s", tc.path, status, raw)
		}
		up := <-received
		if strings.HasPrefix(tc.path, "/unmasked/") {
			if up != message || !bytes.Contains(raw, []byte(message)) {
				t.Fatalf("skip changed body: %s %s", up, raw)
			}
		} else {
			if !token.Valid(up) || !bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte(token.Prefix)) {
				t.Fatalf("client skip header was trusted: %s %s", up, raw)
			}
			marker = up
		}
	}
	// Prefix bypass does not bypass CPA's authentication.
	res, err := http.Post(url+"/unmasked/v1/chat/completions", "application/json", strings.NewReader(`{"model":"fixture-chat","messages":[{"role":"user","content":"PRIVATE"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("bypass skipped host authentication: %d", res.StatusCode)
	}
	for _, path := range []string{"/v1/chat/completions", "/unmasked/v1/chat/completions"} {
		body := []byte(`{"model":"fixture-chat","messages":[{"role":"user","content":"` + strings.Repeat("x", 9<<20) + `"}]}`)
		status, _ := requestHTTP(t, "POST", url+path, body, nil)
		if status != http.StatusRequestEntityTooLarge {
			t.Fatalf("proxy body limit on %s: HTTP %d", path, status)
		}
		select {
		case <-received:
			t.Fatalf("oversized proxy request reached upstream on %s", path)
		default:
		}
	}
}
