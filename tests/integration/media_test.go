//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

func requestHTTP(t *testing.T, method, url string, body []byte, headers http.Header) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Authorization", "Bearer fixture-client")
	if req.Header.Get("Content-Type") == "" && len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, b
}

func TestHostMediaAndSearchBoundaries(t *testing.T) {
	const media = "UFJJVkFURV9NRURJQQ=="
	fileData := []byte{0, 255, 1, 'P', 'R', 'I', 'V', 'A', 'T', 'E'}
	videoData := []byte("\x00fixture-video PRIVATE [[redacted:v1:0123456789abcdef0123456789abcdef]]")
	var mu sync.Mutex
	videoMarker := ""
	var imageCalls, videoCalls, searchCalls, uploadCalls atomic.Int64
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media/video.mp4" {
			w.Header().Set("Content-Type", "video/mp4")
			w.Write(videoData)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/alpha/search") {
			searchCalls.Add(1)
			raw, _ := io.ReadAll(r.Body)
			if !bytes.Contains(raw, []byte(`"PRIVATE"`)) || bytes.Contains(raw, []byte(token.Prefix)) {
				t.Error("search fixture unexpectedly traversed redaction")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"results":[{"title":"PRIVATE","snippet":"PRIVATE"}]}`)
			return
		}
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/videos/") {
			mu.Lock()
			marker := videoMarker
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"request_id": "video_fixture", "status": "done", "prompt": marker, "video": map[string]any{"url": upstream.URL + "/media/video.mp4", "duration": 4}})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		format := "openai-image"
		if strings.Contains(r.URL.Path, "/videos") {
			format = "openai-video"
			videoCalls.Add(1)
		} else {
			imageCalls.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/responses") {
			marker := redactedUpstream(t, "codex", raw)
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(w, map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_image", "output": []any{}}})
			writeSSE(w, map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_image", "created_at": 1, "status": "completed", "output": []any{map[string]any{"type": "image_generation_call", "id": "img_fixture", "status": "completed", "result": media, "revised_prompt": marker, "output_format": "png"}}}})
			return
		}
		doc, err := protocol.ParseRequest(format, raw, r.Header, 1<<20)
		if err != nil {
			t.Errorf("media upstream: %v %s", err, raw)
			w.WriteHeader(400)
			return
		}
		marker := ""
		for _, f := range doc.Fields {
			if !token.Valid(f.Text) {
				t.Errorf("media prompt was not redacted: %s", f.Path)
			}
			marker = f.Text
		}
		if marker == "" {
			t.Error("media upstream missing prompt")
			w.WriteHeader(400)
			return
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			uploadCalls.Add(1)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			defer r.MultipartForm.RemoveAll()
			for _, headers := range r.MultipartForm.File {
				for _, h := range headers {
					f, err := h.Open()
					if err != nil {
						t.Error(err)
						continue
					}
					b, _ := io.ReadAll(f)
					f.Close()
					if !bytes.Equal(b, fileData) || h.Filename != "fixture.png" {
						t.Error("uploaded bytes or filename changed")
					}
				}
			}
		}
		if format == "openai-video" {
			mu.Lock()
			videoMarker = marker
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"request_id": "video_fixture", "status": "done", "prompt": marker, "video": map[string]any{"url": upstream.URL + "/media/video.mp4", "duration": 4}})
			return
		}
		var object map[string]any
		json.Unmarshal(raw, &object)
		if object["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(w, map[string]any{"type": "image_generation.partial_image", "b64_json": media, "partial_image_index": 0})
			writeSSE(w, map[string]any{"type": "image_generation.completed", "b64_json": media, "revised_prompt": marker})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []any{map[string]any{"b64_json": media, "revised_prompt": marker}}})
	}))
	defer upstream.Close()
	providers := fmt.Sprintf(`openai-compatibility:
  - name: media-fixture
    base-url: %q
    api-key-entries: [{api-key: fixture-media}]
    models: [{name: fixture-image, alias: fixture-image, image: true}]
xai-api-key:
  - api-key: fixture-xai
    base-url: %q
    models:
      - {name: grok-imagine-image, alias: grok-imagine-image}
      - {name: grok-imagine-video, alias: grok-imagine-video}
codex-api-key:
  - api-key: fixture-codex
    base-url: %q
    alpha-search: true
    models:
      - {name: gpt-5.4-mini, alias: gpt-5.4-mini}
      - {name: gpt-image-2, alias: gpt-image-2}
`, upstream.URL+"/compat/v1", upstream.URL+"/xai/v1", upstream.URL+"/codex")
	h, probe := startProbedHost(t, hostSettings{Providers: providers, PluginConfig: hostRule + "\n      session:\n        profiles: [{namespace: media, sources: [{header: X-Session-ID}]}]"}, false)
	for _, model := range []string{"fixture-image", "grok-imagine-image", "gpt-image-2"} {
		for _, stream := range []bool{false, true} {
			t.Run(model+fmt.Sprint(stream), func(t *testing.T) {
				start := probe.mark(t)
				body := fmt.Sprintf(`{"model":%q,"prompt":"PRIVATE","stream":%v,"response_format":"b64_json"}`, model, stream)
				status, raw := post(t, h.url+"/v1/images/generations", body, false)
				if status != 200 || bytes.Contains(raw, []byte(token.Prefix)) || !bytes.Contains(raw, []byte(media)) {
					t.Fatalf("%d %s", status, raw)
				}
				// CPA's synthetic xAI image SSE event omits revised_prompt.
				if !(model == "grok-imagine-image" && stream) && !bytes.Contains(raw, []byte("PRIVATE")) {
					t.Fatalf("missing revised prompt: %s", raw)
				}
				probe.verify(t, "images/"+model+"/generate/"+fmt.Sprint(stream), start, "openai-image")
			})
		}
		for _, upload := range []bool{false, true} {
			t.Run(model+"-edit-"+fmt.Sprint(upload), func(t *testing.T) {
				start := probe.mark(t)
				body := []byte(fmt.Sprintf(`{"model":%q,"prompt":"PRIVATE","image":"data:image/png;base64,%s"}`, model, media))
				headers := http.Header{}
				if upload {
					var b bytes.Buffer
					w := multipart.NewWriter(&b)
					w.WriteField("model", model)
					w.WriteField("prompt", "PRIVATE")
					w.WriteField("response_format", "b64_json")
					file, _ := w.CreateFormFile("image", "fixture.png")
					file.Write(fileData)
					w.Close()
					body = b.Bytes()
					headers.Set("Content-Type", w.FormDataContentType())
				}
				status, raw := requestHTTP(t, "POST", h.url+"/v1/images/edits", body, headers)
				if status != 200 || bytes.Contains(raw, []byte(token.Prefix)) || !bytes.Contains(raw, []byte(media)) {
					t.Fatalf("%d %s", status, raw)
				}
				probe.verify(t, "images/"+model+"/edit/"+fmt.Sprint(upload), start, "openai-image")
			})
		}
	}
	// The same image-generation item is also reachable through Responses.
	start := probe.mark(t)
	status, raw := post(t, h.url+"/v1/responses", `{"model":"gpt-5.4-mini","input":"PRIVATE","tools":[{"type":"image_generation"}],"stream":true}`, false)
	if status != 200 || !bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte(token.Prefix)) {
		t.Fatalf("Responses image path: %d %s", status, raw)
	}
	probe.verify(t, "images/responses", start, "openai-response")
	headers := http.Header{"X-Session-ID": {"media-session"}}
	for _, path := range []string{"/v1/videos", "/v1/videos/generations", "/v1/videos/edits", "/v1/videos/extensions"} {
		start = probe.mark(t)
		body := []byte(`{"model":"grok-imagine-video","prompt":"PRIVATE","duration":4,"video":{"url":"https://fixture.invalid/input.mp4"}}`)
		status, raw := requestHTTP(t, "POST", h.url+path, body, headers)
		if status != 200 || !bytes.Contains(raw, []byte(`"prompt":"PRIVATE"`)) || bytes.Contains(raw, []byte(token.Prefix)) {
			t.Fatalf("%s: %d %s", path, status, raw)
		}
		probe.verify(t, path, start, "openai-video")
	}
	start = probe.mark(t)
	status, raw = requestHTTP(t, "POST", h.url+"/openai/v1/videos", []byte(`{"model":"sora-2","prompt":"PRIVATE","seconds":"4","size":"1280x720"}`), headers)
	if status != 200 || bytes.Contains(raw, []byte(token.Prefix)) {
		t.Fatalf("Sora compatibility: %d %s", status, raw)
	}
	probe.verify(t, "/openai/v1/videos", start, "openai-video")
	start = probe.mark(t)
	status, raw = requestHTTP(t, "GET", h.url+"/v1/videos/video_fixture", nil, headers)
	if status != 200 || !bytes.Contains(raw, []byte(`"prompt":"PRIVATE"`)) {
		t.Fatalf("session-bound poll: %d %s", status, raw)
	}
	probe.verify(t, "videos/poll/session", start, "openai-video")
	start = probe.mark(t)
	status, raw = requestHTTP(t, "GET", h.url+"/v1/videos/video_fixture", nil, nil)
	if status != 200 || !bytes.Contains(raw, []byte(token.Prefix)) {
		t.Fatalf("independent poll borrowed another scope: %d %s", status, raw)
	}
	probe.verify(t, "videos/poll/request", start, "openai-video")
	start = probe.mark(t)
	status, raw = requestHTTP(t, "GET", h.url+"/openai/v1/videos/video_fixture/content", nil, headers)
	if status != 200 || !bytes.Equal(raw, videoData) {
		t.Fatalf("binary download: %d %s", status, raw)
	}
	// Download first executes a hooked JSON metadata poll to find the URL;
	// the following media transfer itself is outside the text adapters.
	probe.verify(t, "videos/download-metadata", start, "openai-video")
	for _, path := range []string{"/v1/alpha/search", "/backend-api/codex/alpha/search"} {
		start = probe.mark(t)
		status, raw := post(t, h.url+path, `{"model":"gpt-5.4-mini","query":"PRIVATE"}`, false)
		if status != 200 || !bytes.Contains(raw, []byte("PRIVATE")) {
			t.Fatalf("search boundary: %d %s", status, raw)
		}
		probe.noRequest(t, start, path)
	}
	if imageCalls.Load() < 13 || videoCalls.Load() != 5 || searchCalls.Load() != 2 || uploadCalls.Load() < 1 {
		t.Fatalf("missing media boundary calls: images=%d videos=%d search=%d uploads=%d", imageCalls.Load(), videoCalls.Load(), searchCalls.Load(), uploadCalls.Load())
	}
}
