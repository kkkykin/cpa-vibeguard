package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolResultMediaAndMetadataStayOpaque(t *testing.T) {
	for _, tc := range []struct{ format, raw string }{
		{"openai-response", `{"input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_text","text":"PRIVATE"},{"type":"input_image","image_url":"PRIVATE"},{"type":"input_file","file_data":"PRIVATE","filename":"PRIVATE"}]}]}`},
		{"codex", `{"input":[{"type":"custom_tool_call_output","call_id":"c","output":[{"type":"input_text","text":"PRIVATE"},{"type":"input_audio","input_audio":{"data":"PRIVATE","format":"PRIVATE"}}]}]}`},
		{"openai-response", `{"input":[{"type":"function_call_output","call_id":"c","output":{"content":[{"type":"text","text":"PRIVATE"},{"type":"image","data":"PRIVATE","mimeType":"PRIVATE"}]}}]}`},
		{"interactions", `{"input":[{"type":"function_result","name":"capture","call_id":"c","result":[{"type":"text","text":"PRIVATE"},{"type":"image","data":"PRIVATE","mime_type":"PRIVATE"},{"type":"document","data":"PRIVATE"}]}]}`},
	} {
		t.Run(tc.format+tc.raw, func(t *testing.T) {
			doc, err := Parse(tc.format, []byte(tc.raw), true, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if len(doc.Fields) != 1 || !strings.HasSuffix(doc.Fields[0].Path, "/text") {
				t.Fatalf("selected media or metadata: %+v", doc.Fields)
			}
			doc.Fields[0].Set("MASKED")
			out, err := doc.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			var expected any
			if err := json.Unmarshal([]byte(strings.ReplaceAll(tc.raw, `"text":"PRIVATE"`, `"text":"MASKED"`)), &expected); err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(expected)
			if string(out) != string(want) {
				t.Fatalf("opaque result changed: %s", out)
			}
		})
	}
}

func TestToolArgumentsDoNotInferMediaFromTypeProperty(t *testing.T) {
	doc, err := Parse("openai-response", []byte(`{"input":[{"type":"function_call","arguments":"{\"type\":\"image\",\"data\":\"PRIVATE\"}"}]}`), true, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Fields) != 2 {
		t.Fatal("arbitrary argument values skipped based on type")
	}
}

func TestOrdinaryToolResultsWithTypePropertiesStillScan(t *testing.T) {
	for _, typ := range []string{"text", "image", "file", "audio", "document"} {
		raw := []byte(`{"input":[{"type":"function_call_output","output":{"type":"` + typ + `","value":"PRIVATE"}}]}`)
		doc, err := Parse("openai-response", raw, true, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if len(doc.Fields) != 2 || doc.Fields[1].Text != "PRIVATE" {
			t.Fatal("ordinary result was mistaken for protocol content")
		}
	}
}
