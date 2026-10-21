package plugin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/document"
	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/testutil"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func mediaStringAt(t *testing.T, raw []byte, path string) string {
	t.Helper()
	v, err := document.JSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	v, exists, err := document.Pointer(v, path)
	if err != nil || !exists {
		t.Fatalf("pointer %s: %v", path, err)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("not string at %s", path)
	}
	return s
}

func TestRequestPreservesBase64Images(t *testing.T) {
	_, b64 := testutil.PNGWithPhone(t)
	e := mediaEngine(t)
	cases := []struct {
		name, format, raw, path, nested string
	}{
		{"chat_image", "openai", `{"messages":[{"role":"user","content":[{"type":"text","text":"13800138000"},{"type":"image_url","image_url":{"url":"data:image/png;base64,PNG"}}]}]}`, "/messages/0/content/1/image_url/url", ""},
		{"responses_image", "openai-response", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,PNG"}]}]}`, "/input/0/content/0/image_url", ""},
		{"claude_image", "claude", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"PNG"}}]}]}`, "/messages/0/content/0/source/data", ""},
		{"claude_document", "claude", `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"PNG"}}]}]}`, "/messages/0/content/0/source/data", ""},
		{"claude_tool_result", "claude", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"PNG"}}]}]}]}`, "/messages/0/content/0/content/0/source/data", ""},
		{"gemini_image", "gemini", `{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"PNG"}}]}]}`, "/contents/0/parts/0/inlineData/data", ""},
		{"gemini_tool_parts", "gemini", `{"contents":[{"parts":[{"functionResponse":{"name":"capture","response":{"ok":true},"parts":[{"inlineData":{"mimeType":"image/png","data":"PNG"}}]}}]}]}`, "/contents/0/parts/0/functionResponse/parts/0/inlineData/data", ""},
		{"interactions_image", "interactions", `{"input":[{"type":"image","mime_type":"image/png","data":"PNG"}]}`, "/input/0/data", ""},
		{"images_edit", "openai-image", `{"prompt":"13800138000","image":"data:image/png;base64,PNG"}`, "/image", ""},
		{"videos_reference", "openai-video", `{"prompt":"13800138000","image":"data:image/png;base64,PNG"}`, "/image", ""},
		{"responses_tool_output", "openai-response", `{"input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"data:image/png;base64,PNG"}]}]}`, "/input/0/output/0/image_url", ""},
		{"responses_custom_output", "openai-response", `{"input":[{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"data:image/png;base64,PNG"}]}]}`, "/input/0/output/0/image_url", ""},
		{"responses_tool_arguments", "openai-response", `{"input":[{"type":"function_call","name":"upload","call_id":"c1","arguments":"{\"image_base64\":\"PNG\"}"}]}`, "/input/0/arguments", "/image_base64"},
		{"claude_tool_input", "claude", `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"upload","input":{"image_base64":"PNG"}}]}]}`, "/messages/0/content/0/input/image_base64", ""},
		{"gemini_tool_args", "gemini", `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"upload","args":{"image_base64":"PNG"}}}]}]}`, "/contents/0/parts/0/functionCall/args/image_base64", ""},
		{"interactions_tool_result", "interactions", `{"input":[{"type":"function_result","call_id":"c1","name":"capture","result":[{"type":"image","mime_type":"image/png","data":"PNG"}]}]}`, "/input/0/result/0/data", ""},
		{"markdown_image", "openai", `{"messages":[{"role":"user","content":"![screenshot](data:image/png;base64,PNG)"}]}`, "/messages/0/content", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, textPath := mediaRequestText(t, tc.format, []byte(strings.ReplaceAll(tc.raw, "PNG", b64)))
			before := mediaStringAt(t, raw, tc.path)
			if tc.nested != "" {
				before = mediaStringAt(t, []byte(before), tc.nested)
			}
			res := e.Before(pluginapi.RequestInterceptRequest{RequestID: tc.name, SourceFormat: tc.format, Body: raw})
			defer e.Complete(tc.name)
			if res.Terminate || len(res.Body) == 0 {
				t.Fatalf("request rejected: %s", res.ResponseBody)
			}
			after := mediaStringAt(t, res.Body, tc.path)
			if tc.nested != "" {
				after = mediaStringAt(t, []byte(after), tc.nested)
			}
			if before != after {
				t.Fatal("image payload changed")
			}
			if !token.Valid(mediaStringAt(t, res.Body, textPath)) {
				t.Fatal("surrounding text was not redacted")
			}
			encoded := after
			if i := strings.Index(encoded, "base64,"); i >= 0 {
				encoded = encoded[i+7:]
			}
			encoded = strings.TrimSuffix(encoded, ")")
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := png.Decode(bytes.NewReader(decoded)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMultipartRPCPreservesBinaryFile(t *testing.T) {
	pngBytes, _ := testutil.PNGWithPhone(t)
	binaryData := append(append([]byte(nil), pngBytes...), []byte("\x00\xff13800138000\r\n")...)
	e := mediaEngine(t)
	var raw bytes.Buffer
	w := multipart.NewWriter(&raw)
	if err := w.WriteField("prompt", "13800138000"); err != nil {
		t.Fatal(err)
	}
	f, err := w.CreateFormFile("image", "13800138000.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(binaryData); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := pluginapi.RequestInterceptRequest{RequestID: "multipart-rpc", SourceFormat: "openai-image", Body: raw.Bytes(), Headers: http.Header{"Content-Type": {w.FormDataContentType()}}}
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(e.Handle(pluginabi.MethodRequestInterceptBefore, encoded), &envelope); err != nil || !envelope.OK {
		t.Fatalf("RPC failed: %v", err)
	}
	var res pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(envelope.Result, &res); err != nil || res.Terminate {
		t.Fatalf("hook failed: %v %s", err, res.ResponseBody)
	}
	r := multipart.NewReader(bytes.NewReader(res.Body), w.Boundary())
	foundFile, foundPrompt := false, false
	for {
		p, err := r.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
		if p.FormName() == "image" {
			foundFile = true
			if !bytes.Equal(data, binaryData) || p.FileName() != "13800138000.png" {
				t.Fatal("binary file or name changed")
			}
		}
		if p.FormName() == "prompt" {
			foundPrompt = true
			if !token.Valid(string(data)) {
				t.Fatal("prompt not redacted")
			}
		}
	}
	if !foundFile || !foundPrompt {
		t.Fatal("missing form part")
	}
	t.Log("binary bytes, NUL, invalid UTF-8, CRLF and filename preserved through RPC; prompt redacted")
}

func TestImageResponsesPreserveBase64(t *testing.T) {
	_, b64 := testutil.PNGWithPhone(t)
	e := mediaEngine(t)
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "stream"}[stream], func(t *testing.T) {
			id := t.Name()
			req := e.Before(pluginapi.RequestInterceptRequest{RequestID: id, SourceFormat: "openai-image", Body: []byte(`{"prompt":"13800138000"}`)})
			if req.Terminate {
				t.Fatal("rejected")
			}
			marker := mediaStringAt(t, req.Body, "/prompt")
			var raw []byte
			var path string
			if stream {
				raw, _ = json.Marshal(map[string]any{"type": "image_generation.completed", "b64_json": b64, "revised_prompt": marker})
				path = "/b64_json"
			} else {
				raw, _ = json.Marshal(map[string]any{"data": []any{map[string]any{"b64_json": b64, "revised_prompt": marker}}})
				path = "/data/0/b64_json"
			}
			var out []byte
			if stream {
				out = e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: id, SourceFormat: "openai-image", ChunkIndex: 0, Body: append(append([]byte("data: "), raw...), '\n', '\n')}).Body
				out = bytes.TrimSpace(bytes.TrimPrefix(out, []byte("data: ")))
			} else {
				out = e.Response(pluginapi.ResponseInterceptRequest{RequestID: id, SourceFormat: "openai-image", Body: raw}).Body
			}
			if len(out) == 0 {
				t.Fatal("no restored output")
			}
			if mediaStringAt(t, out, path) != b64 {
				t.Fatal("image changed")
			}
			doc, err := protocol.Parse("openai-image", out, false, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if len(doc.Fields) != 1 || doc.Fields[0].Text != "13800138000" {
				t.Fatal("prompt not restored")
			}
			e.Complete(id)
		})
	}
}

func TestToolResultMediaStaysOpaqueDuringRestoration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "snapshot"}[stream], func(t *testing.T) {
			e := mediaEngine(t)
			request := e.Before(pluginapi.RequestInterceptRequest{RequestID: "media", SourceFormat: "openai-response", Body: []byte(`{"input":"13800138000"}`)})
			if request.Terminate {
				t.Fatal("request rejected")
			}
			marker := mediaStringAt(t, request.Body, "/input")
			// Markers in opaque fields must stay literal, including in complete
			// stream snapshots which share the ordinary response field codecs.
			response := map[string]any{"output": []any{map[string]any{
				"type": "mcp_call", "arguments": "{}", "output": []any{
					map[string]any{"type": "text", "text": marker},
					map[string]any{"type": "image", "data": marker, "mimeType": "image/png"},
					map[string]any{"type": "input_file", "file_data": marker, "filename": marker},
				},
			}}}
			raw, _ := json.Marshal(response)
			path := "/output/0/output"
			var out []byte
			if stream {
				raw, _ = json.Marshal(map[string]any{"type": "response.completed", "response": response})
				out = e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: "media", SourceFormat: "openai-response", Body: raw}).Body
				path = "/response" + path
			} else {
				out = e.Response(pluginapi.ResponseInterceptRequest{RequestID: "media", SourceFormat: "openai-response", Body: raw}).Body
			}
			if mediaStringAt(t, out, path+"/0/text") != "13800138000" || mediaStringAt(t, out, path+"/1/data") != marker || mediaStringAt(t, out, path+"/2/file_data") != marker || mediaStringAt(t, out, path+"/2/filename") != marker {
				t.Fatal("restoration modified opaque media or lost text")
			}
		})
	}
}

func mediaEngine(t *testing.T) *Engine {
	t.Helper()
	path, err := filepath.Abs("../../examples/rules/pii.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"rule_sources": []any{map[string]any{"file": path}}})
	if err != nil {
		t.Fatal(err)
	}
	return configured(t, string(raw))
}

func mediaRequestText(t *testing.T, format string, raw []byte) ([]byte, string) {
	t.Helper()
	v, err := document.JSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	root := v.(map[string]any)
	path := ""
	const secret = "13800138000"
	switch format {
	case "openai", "claude":
		messages := root["messages"].([]any)
		path = "/messages/1/content"
		if len(messages) != 1 {
			t.Fatal("fixture must have one message")
		}
		root["messages"] = append(messages, map[string]any{"role": "user", "content": secret})
	case "openai-response", "codex":
		root["instructions"] = secret
		path = "/instructions"
	case "gemini":
		root["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": secret}}}
		path = "/systemInstruction/parts/0/text"
	case "interactions":
		root["system_instruction"] = secret
		path = "/system_instruction"
	case "openai-image", "openai-video":
		root["prompt"] = secret
		path = "/prompt"
	default:
		t.Fatal("unsupported fixture format")
	}
	raw, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return raw, path
}
