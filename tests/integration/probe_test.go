//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var probeBuild struct {
	sync.Once
	dir string
	err error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if probeBuild.dir != "" {
		os.RemoveAll(probeBuild.dir)
	}
	os.Exit(code)
}

func probeBinary(t *testing.T) string {
	t.Helper()
	probeBuild.Do(func() {
		probeBuild.dir, probeBuild.err = os.MkdirTemp("", "vibeguard-probe-build-")
		if probeBuild.err != nil {
			return
		}
		cmd := exec.Command("go", "build", "-tags=integration", "-buildmode=c-shared", "-o", filepath.Join(probeBuild.dir, "probe.so"), "./tests/integration/probe")
		cmd.Dir = "../.."
		cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
		if raw, err := cmd.CombinedOutput(); err != nil {
			probeBuild.err = fmt.Errorf("probe build: %w: %s", err, raw)
		}
	})
	if probeBuild.err != nil {
		t.Fatal(probeBuild.err)
	}
	return filepath.Join(probeBuild.dir, "probe.so")
}

type hookRecord struct {
	Method           string   `json:"method"`
	RequestID        string   `json:"request_id"`
	SourceFormat     string   `json:"source_format"`
	ToFormat         string   `json:"to_format"`
	Path             string   `json:"path"`
	Shape            string   `json:"shape"`
	Keys             []string `json:"keys"`
	Outcome          string   `json:"outcome"`
	ChunkIndex       int      `json:"chunk_index"`
	OriginalBytes    int      `json:"original_bytes"`
	HistoryChunks    int      `json:"history_chunks"`
	CallerPresent    bool     `json:"caller_present"`
	ExecutionPresent bool     `json:"execution_session_present"`
}
type hookProbe struct{ path string }

func startProbedHost(t *testing.T, settings hostSettings, onlyProbe bool) (*hostProcess, *hookProbe) {
	t.Helper()
	dir := t.TempDir()
	name := "cpa-vibeguard-probe.so"
	if onlyProbe {
		name = "cpa-vibeguard.so"
	} else {
		copyPlugin(t, dir, "cpa-vibeguard.so")
		settings.PluginConfig += "\n    cpa-vibeguard-probe:\n      enabled: true\n      priority: 10000"
	}
	raw, err := os.ReadFile(probeBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	atomicFile(t, filepath.Join(dir, name), raw)
	p := &hookProbe{path: filepath.Join(dir, "hooks.ndjson")}
	settings.PluginDir = dir
	settings.Env = append(settings.Env, "VIBEGUARD_PROBE_LOG="+p.path)
	return startHostConfigured(t, settings), p
}

func (p *hookProbe) records(t *testing.T) []hookRecord {
	t.Helper()
	raw, err := os.ReadFile(p.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var records []hookRecord
	// Ignore an incomplete last write; a subsequent read will include it.
	lines := bytes.Split(raw, []byte("\n"))
	for _, line := range lines[:len(lines)-1] {
		if len(line) == 0 {
			continue
		}
		var r hookRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}

func (p *hookProbe) mark(t *testing.T) int { return len(p.records(t)) }

func (p *hookProbe) completed(t *testing.T, start int) []hookRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		records := p.records(t)[start:]
		id := ""
		var request []hookRecord
		for _, r := range records {
			if id == "" && r.Method == "request.intercept_before" {
				id = r.RequestID
			}
			if id != "" && r.RequestID == id {
				request = append(request, r)
				if r.Method == "request.complete" {
					return request
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing completed hook sequence: %+v", records)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type entrypointRecord struct {
	Name           string   `json:"name"`
	SourceFormat   string   `json:"source_format"`
	ToFormats      []string `json:"to_formats"`
	Path           string   `json:"path"`
	RequestShape   string   `json:"request_shape"`
	RequestKeys    []string `json:"request_keys"`
	ResponseShapes []string `json:"response_shapes"`
	Hooks          []string `json:"hooks"`
}

func (p *hookProbe) verify(t *testing.T, name string, start int, format string) {
	t.Helper()
	sequence := p.completed(t, start)
	first := sequence[0]
	if strings.HasPrefix(name, "websocket/") && !first.ExecutionPresent {
		t.Fatal("WebSocket hook has no execution_session_id")
	}
	entry := entrypointRecord{Name: name, SourceFormat: first.SourceFormat, Path: first.Path, RequestShape: first.Shape, RequestKeys: first.Keys}
	to, shapes := map[string]bool{}, map[string]bool{}
	for _, r := range sequence {
		if r.SourceFormat != format || r.RequestID == "" || !r.CallerPresent {
			t.Fatalf("unexpected hook context: %+v", r)
		}
		method := r.Method
		switch method {
		case "request.intercept_before":
			if r.ToFormat != "" {
				t.Fatal("before-auth hook already selected an upstream format")
			}
		case "request.intercept_after":
			to[r.ToFormat] = true
		case "response.intercept_after":
			shapes[r.Shape] = true
		case "response.intercept_stream_chunk":
			if r.ChunkIndex == -1 {
				method = "response.stream_headers"
			} else {
				shapes[r.Shape] = true
				if r.OriginalBytes != 0 || r.HistoryChunks != 0 {
					t.Fatal("schema 6 payload chunk copied original request/history")
				}
			}
		}
		if len(entry.Hooks) == 0 || entry.Hooks[len(entry.Hooks)-1] != method {
			entry.Hooks = append(entry.Hooks, method)
		}
	}
	for value := range to {
		entry.ToFormats = append(entry.ToFormats, value)
	}
	for value := range shapes {
		entry.ResponseShapes = append(entry.ResponseShapes, value)
	}
	sort.Strings(entry.ToFormats)
	sort.Strings(entry.ResponseShapes)
	verifyInventory(t, entry)
}

func verifyInventory(t *testing.T, entry entrypointRecord) {
	t.Helper()
	if os.Getenv("VIBEGUARD_RECORD_INVENTORY") == "1" {
		raw, _ := json.Marshal(entry)
		t.Logf("ENTRYPOINT %s", raw)
		return
	}
	raw, err := os.ReadFile("../../testdata/entrypoints.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []entrypointRecord
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, expected := range fixtures {
		if expected.Name == entry.Name {
			if !reflect.DeepEqual(entry, expected) {
				t.Fatalf("entrypoint changed:\ngot  %+v\nwant %+v", entry, expected)
			}
			return
		}
	}
	t.Fatalf("unrecorded entrypoint %s", entry.Name)
}

func (p *hookProbe) noRequest(t *testing.T, start int, paths ...string) {
	t.Helper()
	for _, r := range p.records(t)[start:] {
		if strings.HasPrefix(r.Method, "request.intercept_") {
			t.Fatalf("unexpected request hook: %+v", r)
		}
	}
	for _, path := range paths {
		verifyInventory(t, entrypointRecord{Name: "unintercepted/" + path, Path: path})
	}
}

func TestHostRPCFaultIsSkipped(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Contains(raw, []byte("PRIVATE")) {
			t.Error("RPC-fault request did not reach upstream unchanged")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"fault","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"PRIVATE"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	providers := fmt.Sprintf("openai-compatibility:\n  - name: fault\n    base-url: %q\n    api-key-entries: [{api-key: fixture}]\n    models: [{name: fixture-chat}]\n", upstream.URL+"/v1")
	h, probe := startProbedHost(t, hostSettings{Providers: providers}, true)
	body := []byte(`{"model":"fixture-chat","messages":[{"role":"user","content":"PRIVATE"}]}`)
	start := probe.mark(t)
	status, raw := requestHTTP(t, "POST", h.url+"/v1/chat/completions", body, http.Header{"X-Fixture-Mode": {"rpc-error"}})
	if status != 200 || !bytes.Contains(raw, []byte("PRIVATE")) || calls.Load() != 1 {
		t.Fatalf("RPC-fault boundary: %d %s", status, raw)
	}
	sequence := probe.completed(t, start)
	if len(sequence) != 4 || sequence[1].Method != "request.intercept_after" || sequence[3].Outcome != "succeeded" {
		t.Fatalf("RPC errors were not skipped: %+v", sequence)
	}
	h.awaitLog(t, 0, "fixture_rpc_error")
	start = probe.mark(t)
	status, raw = requestHTTP(t, "POST", h.url+"/v1/chat/completions", body, http.Header{"X-Fixture-Mode": {"reject"}})
	if status != 422 || !bytes.Contains(raw, []byte("fixture_business_rejection")) || calls.Load() != 1 {
		t.Fatalf("business rejection boundary: %d %s", status, raw)
	}
	sequence = probe.completed(t, start)
	if len(sequence) != 2 || sequence[1].Outcome != "rejected" {
		t.Fatalf("business termination hook sequence: %+v", sequence)
	}
}
