//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kkkykin/cpa-vibeguard/internal/document"
	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

type protocolFixture struct {
	Name, Format      string
	Request, Response json.RawMessage
}
type streamFixture struct {
	Name, Format, Framing string
	Request               json.RawMessage
	Events                []json.RawMessage
}

func loadProtocolFixtures(t *testing.T) map[string]protocolFixture {
	t.Helper()
	b, err := os.ReadFile("../../testdata/protocols.json")
	if err != nil {
		t.Fatal(err)
	}
	var all []protocolFixture
	if err = json.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	m := make(map[string]protocolFixture)
	for _, f := range all {
		m[f.Name] = f
	}
	return m
}
func loadStreamFixtures(t *testing.T) map[string]streamFixture {
	t.Helper()
	b, err := os.ReadFile("../../testdata/streams.json")
	if err != nil {
		t.Fatal(err)
	}
	var all []streamFixture
	if err = json.Unmarshal(b, &all); err != nil {
		t.Fatal(err)
	}
	m := make(map[string]streamFixture)
	for _, f := range all {
		m[f.Format] = f
	}
	return m
}
func fixtureRequest(t *testing.T, raw []byte, model string, stream bool) string {
	t.Helper()
	v, err := document.JSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	delete(m, "previous_response_id")
	delete(m, "previous_interaction_id")
	if model != "" {
		m["model"] = model
	}
	if stream {
		m["stream"] = true
	}
	if m["messages"] != nil {
		m["max_tokens"] = 1024
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func redactedUpstream(t *testing.T, format string, raw []byte) string {
	t.Helper()
	doc, err := protocol.Parse(format, raw, true, 1<<20)
	if err != nil {
		t.Errorf("invalid %s upstream fixture: %v %s", format, err, raw)
		return ""
	}
	marker := ""
	for _, f := range doc.Fields {
		if strings.Contains(f.Text, "PRIVATE") {
			t.Errorf("plaintext reached %s upstream field %s", format, f.Path)
		}
		if marker == "" {
			start := strings.Index(f.Text, token.Prefix)
			if start >= 0 && len(f.Text) >= start+token.Length {
				candidate := f.Text[start : start+token.Length]
				if token.Valid(candidate) {
					marker = candidate
				}
			}
		}
	}
	if marker == "" {
		t.Errorf("%s upstream contains no authorized marker", format)
	}
	return marker
}
func maskedResponse(t *testing.T, f protocolFixture, marker string) []byte {
	t.Helper()
	d, err := protocol.Parse(f.Format, f.Response, false, 1<<20)
	if err != nil {
		t.Error(err)
		return nil
	}
	for _, field := range d.Fields {
		if field.Text == "PRIVATE" {
			field.Set(marker)
		}
	}
	b, err := d.Marshal()
	if err != nil {
		t.Error(err)
	}
	return b
}
func emitFixture(w http.ResponseWriter, f streamFixture, marker string, split int) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range f.Events {
		raw := strings.NewReplacer("@MARKER@", marker, "@HEAD@", marker[:split], "@TAIL@", marker[split:]).Replace(string(event))
		var m map[string]any
		json.Unmarshal([]byte(raw), &m)
		name, _ := m["type"].(string)
		if name == "" {
			name, _ = m["event_type"].(string)
		}
		if name != "" {
			fmt.Fprintf(w, "event: %s\n", name)
		}
		fmt.Fprintf(w, "data: %s\n\n", raw)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	if f.Format == "openai" {
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}
func nativeProviders(base string) string {
	return fmt.Sprintf(`claude-api-key:
  - api-key: fixture-claude
    base-url: %q
    models: [{name: claude-sonnet-4-5, alias: native-claude, is-compat: true}]
gemini-api-key:
  - api-key: fixture-gemini
    base-url: %q
    models: [{name: gemini-2.5-flash, alias: native-gemini, is-compat: true}]
interactions-api-key:
  - api-key: fixture-interactions
    base-url: %q
    models: [{name: gemini-2.5-flash, alias: native-interactions, is-compat: true}]
codex-api-key:
  - api-key: fixture-codex
    base-url: %q
    alpha-search: true
    models: [{name: gpt-5-codex, alias: native-codex, is-compat: true}]
openai-compatibility:
  - name: fixture
    base-url: %q
    api-key-entries: [{api-key: fixture-openai}]
    models: [{name: fixture-chat, alias: fixture-chat}]
`, base+"/claude", base+"/gemini", base+"/interactions", base+"/codex", base+"/openai/v1")
}

func TestHostNativeAndCrossProtocolMatrix(t *testing.T) {
	fixtures := loadProtocolFixtures(t)
	streams := loadStreamFixtures(t)
	var split atomic.Int64
	split.Store(20)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		format, name := "", ""
		switch {
		case strings.HasPrefix(r.URL.Path, "/claude/"):
			format, name = "claude", "claude"
		case strings.HasPrefix(r.URL.Path, "/gemini/"):
			format, name = "gemini", "gemini"
		case strings.HasPrefix(r.URL.Path, "/interactions/"):
			format, name = "interactions", "interactions"
		case strings.HasPrefix(r.URL.Path, "/codex/"):
			format, name = "codex", "responses"
		case strings.HasPrefix(r.URL.Path, "/openai/"):
			format, name = "openai", "chat"
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/responses/compact") {
			format, name = "openai-response", "compact"
		}
		marker := redactedUpstream(t, format, raw)
		if marker == "" {
			w.WriteHeader(400)
			return
		}
		if strings.HasSuffix(r.URL.Path, ":countTokens") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"totalTokens":42}`)
			return
		}
		if body["stream"] == true || strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			if format == "codex" {
				format = "openai-response"
			}
			emitFixture(w, streams[format], marker, int(split.Load()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(maskedResponse(t, fixtures[name], marker))
	}))
	defer upstream.Close()
	h, probe := startProbedHost(t, hostSettings{Providers: nativeProviders(upstream.URL), PluginConfig: hostRule}, false)
	ordinary := []struct{ name, path, model, fixture string }{
		{"claude", "/v1/messages", "native-claude", "claude"},
		{"gemini", "/v1beta/models/native-gemini:generateContent", "", "gemini"},
		{"gemini-count", "/v1beta/models/native-gemini:countTokens", "", "gemini-count"},
		{"interactions-native", "/v1beta/interactions", "native-interactions", "interactions"},
		{"interactions-gemini", "/v1beta/interactions", "native-gemini", "interactions"},
		{"responses-codex", "/v1/responses", "native-codex", "responses"},
		{"codex-alias", "/backend-api/codex/responses", "native-codex", "responses"},
		{"compact", "/v1/responses/compact", "native-codex", "compact"},
		{"compact-alias", "/backend-api/codex/responses/compact", "native-codex", "compact"},
		{"compact-compatible", "/v1/responses/compact", "fixture-chat", "compact"},
		{"chat-gemini", "/v1/chat/completions", "native-gemini", "chat"},
		{"claude-chat", "/v1/messages", "fixture-chat", "claude"},
		{"responses-chat", "/v1/responses", "fixture-chat", "responses"},
	}
	for _, tc := range ordinary {
		t.Run(tc.name, func(t *testing.T) {
			start := probe.mark(t)
			before := calls.Load()
			status, raw := post(t, h.url+tc.path, fixtureRequest(t, fixtures[tc.fixture].Request, tc.model, false), false)
			if status != 200 {
				t.Fatalf("%d %s", status, raw)
			}
			if calls.Load() != before+1 {
				t.Fatal("expected one native/compatible upstream call")
			}
			if !json.Valid(raw) || bytes.Contains(raw, []byte(token.Prefix)) {
				t.Fatalf("response was not restored: %s", raw)
			}
			parsed, err := protocol.Parse(fixtures[tc.fixture].Format, raw, false, 1<<20)
			if err != nil {
				t.Fatalf("client protocol: %v %s", err, raw)
			}
			if tc.fixture != "gemini-count" {
				found := false
				for _, field := range parsed.Fields {
					if strings.Contains(field.Text, "PRIVATE") {
						found = true
					}
				}
				if !found {
					t.Fatal("selected response fields contain no restored text")
				}
			}
			if tc.fixture == "gemini-count" {
				if !bytes.Contains(raw, []byte("42")) {
					t.Fatal(string(raw))
				}
			} else if !bytes.Contains(raw, []byte("PRIVATE")) {
				t.Fatal("missing restored response text")
			}
			probe.verify(t, "matrix/"+tc.name, start, fixtures[tc.fixture].Format)
		})
	}
	streamTests := []struct{ name, path, model, format string }{
		{"claude-stream", "/v1/messages", "native-claude", "claude"},
		{"gemini-sse", "/v1beta/models/native-gemini:streamGenerateContent", "", "gemini"},
		{"gemini-alt-sse", "/v1beta/models/native-gemini:streamGenerateContent?alt=sse", "", "gemini"},
		{"interactions-native-stream", "/v1beta/interactions", "native-interactions", "interactions"},
		{"interactions-gemini-stream", "/v1beta/interactions", "native-gemini", "interactions"},
		{"responses-native-stream", "/v1/responses", "native-codex", "openai-response"},
		{"codex-alias-stream", "/backend-api/codex/responses", "native-codex", "openai-response"},
		{"chat-gemini-stream", "/v1/chat/completions", "native-gemini", "openai"},
		{"claude-chat-stream", "/v1/messages", "fixture-chat", "claude"},
		{"responses-chat-stream", "/v1/responses", "fixture-chat", "openai-response"},
	}
	for _, tc := range streamTests {
		t.Run(tc.name, func(t *testing.T) {
			for n := 0; n <= token.Length; n++ {
				start := 0
				if n == 0 {
					start = probe.mark(t)
				}
				split.Store(int64(n))
				status, raw := post(t, h.url+tc.path, fixtureRequest(t, streams[tc.format].Request, tc.model, true), false)
				if status != 200 || !bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte(token.Prefix)) {
					t.Fatalf("split %d: %d %s", n, status, raw)
				}
				events := streamObjects(t, raw)
				if len(events) == 0 {
					t.Fatalf("no SSE output: %s", raw)
				}
				assertStreamText(t, tc.format, events)
				if n == 0 {
					probe.verify(t, "matrix/"+tc.name, start, tc.format)
				}
			}
		})
	}
}

func assertStreamText(t *testing.T, format string, events []map[string]any) {
	t.Helper()
	text := make(map[string]string)
	add := func(key string, v any) {
		if s, ok := v.(string); ok {
			text[key] += s
		}
	}
	for _, m := range events {
		switch format {
		case "openai":
			a, _ := m["choices"].([]any)
			for _, v := range a {
				c := v.(map[string]any)
				d, _ := c["delta"].(map[string]any)
				add(fmt.Sprint(c["index"])+"/text", d["content"])
				calls, _ := d["tool_calls"].([]any)
				for _, v := range calls {
					call := v.(map[string]any)
					f, _ := call["function"].(map[string]any)
					add(fmt.Sprint(c["index"])+"/tool/"+fmt.Sprint(call["index"]), f["arguments"])
				}
			}
		case "claude":
			if m["type"] == "content_block_delta" {
				d, _ := m["delta"].(map[string]any)
				for _, key := range []string{"text", "thinking", "partial_json"} {
					add(fmt.Sprint(m["index"])+"/"+key, d[key])
				}
			}
		case "openai-response":
			name, _ := m["type"].(string)
			if strings.HasSuffix(name, ".delta") {
				add(name+"/"+fmt.Sprint(m["output_index"])+"/"+fmt.Sprint(m["content_index"]), m["delta"])
			}
		case "interactions":
			if m["event_type"] == "step.delta" {
				d, _ := m["delta"].(map[string]any)
				for _, key := range []string{"text", "arguments"} {
					add(fmt.Sprint(m["index"])+"/"+key, d[key])
				}
				if c, ok := d["content"].(map[string]any); ok {
					add(fmt.Sprint(m["index"])+"/thought", c["text"])
				}
			}
		case "gemini":
			a, _ := m["candidates"].([]any)
			for _, v := range a {
				c := v.(map[string]any)
				content, _ := c["content"].(map[string]any)
				parts, _ := content["parts"].([]any)
				for j, v := range parts {
					p := v.(map[string]any)
					add(fmt.Sprint(c["index"])+"/"+fmt.Sprint(j)+"/"+fmt.Sprint(p["thought"]), p["text"])
				}
			}
		}
	}
	found := false
	for key, value := range text {
		if strings.Contains(value, token.Prefix) {
			t.Fatalf("unrestored accumulated field %s: %s", key, value)
		}
		if strings.Contains(value, "PRIVATE") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no restored deltas in %s stream: %v", format, text)
	}
}

func TestHostClaudeNativeCountAndLocalCount(t *testing.T) {
	var upstreamCalls atomic.Int64
	// A local CONNECT fixture exercises CPA's first-party HTTPS-only count
	// branch. The temporary certificate is trusted by this child host alone.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"api.anthropic.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(t.TempDir(), "fixture-ca.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "api.anthropic.com:443" {
			t.Errorf("unexpected proxy destination %s %s", r.Method, r.Host)
			w.WriteHeader(400)
			return
		}
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(buffer, "HTTP/1.1 200 Connection established\r\n\r\n")
		buffer.Flush()
		secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
		secure.SetDeadline(time.Now().Add(10 * time.Second))
		if err := secure.Handshake(); err != nil {
			t.Error(err)
			return
		}
		r, err = http.ReadRequest(bufio.NewReader(secure))
		if err != nil {
			t.Error(err)
			return
		}
		defer r.Body.Close()
		upstreamCalls.Add(1)
		if r.Host != "api.anthropic.com" || r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("unexpected proxy request %s", r.URL)
		}
		raw, _ := io.ReadAll(r.Body)
		redactedUpstream(t, "claude", raw)
		payload := `{"input_tokens":42}`
		response := http.Response{StatusCode: 200, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(payload)), ContentLength: int64(len(payload)), Close: true}
		if err := response.Write(secure); err != nil {
			t.Error(err)
		}
	}))
	defer proxy.Close()
	providers := fmt.Sprintf(`claude-api-key:
  - api-key: fixture-count
    base-url: https://api.anthropic.com
    proxy-url: %q
    models: [{name: claude-sonnet-4-5, alias: count-native}]
  - api-key: fixture-local
    base-url: %q
    models: [{name: claude-sonnet-4-5, alias: count-local}]
`, proxy.URL, proxy.URL)
	h, probe := startProbedHost(t, hostSettings{Providers: providers, PluginConfig: hostRule + "\n      bypass: {headers: [{name: X-Vibeguard-Skip, value: 'true'}]}", Env: []string{"SSL_CERT_FILE=" + certPath}}, false)
	start := probe.mark(t)
	status, raw := post(t, h.url+"/v1/messages/count_tokens", `{"model":"count-native","messages":[{"role":"user","content":"PRIVATE"}]}`, false)
	if status != 200 || !bytes.Contains(raw, []byte("42")) || upstreamCalls.Load() != 1 {
		t.Fatalf("native count %d %s calls=%d", status, raw, upstreamCalls.Load())
	}
	probe.verify(t, "count/claude-native", start, "claude")
	counts := make([]int, 2)
	for i, skip := range []bool{false, true} {
		start = probe.mark(t)
		status, raw = post(t, h.url+"/v1/messages/count_tokens", `{"model":"count-local","messages":[{"role":"user","content":"PRIVATE"}]}`, skip)
		var result struct {
			InputTokens int `json:"input_tokens"`
		}
		json.Unmarshal(raw, &result)
		counts[i] = result.InputTokens
		if status != 200 || result.InputTokens <= 0 {
			t.Fatalf("local count %d %s", status, raw)
		}
		probe.verify(t, "count/claude-local-"+fmt.Sprint(skip), start, "claude")
	}
	if upstreamCalls.Load() != 1 || counts[0] <= counts[1] {
		t.Fatalf("local counter did not see marker expansion: %v calls=%d", counts, upstreamCalls.Load())
	}
}
