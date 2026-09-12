package protocol

import (
	"encoding/json"
	"testing"
)

func TestCustomToolDescriptionsAndOpaqueDefinitions(t *testing.T) {
	for _, format := range []string{"openai", "openai-response", "codex"} {
		t.Run(format, func(t *testing.T) {
			definition := map[string]any{"name": "PRIVATE_TOOL", "description": "PRIVATE", "format": map[string]any{"type": "grammar", "grammar": map[string]any{"syntax": "regex", "definition": "PRIVATE"}}}
			tool := map[string]any{"type": "custom", "custom": definition}
			request := map[string]any{"tools": []any{tool}}
			if format == "openai" {
				request["messages"] = []any{map[string]any{"role": "user", "content": "ok"}}
			} else {
				// The flat Responses representation must keep working too.
				definition["type"] = "custom"
				request["tools"] = []any{tool, definition}
				request["input"] = "ok"
			}
			raw, _ := json.Marshal(request)
			doc, err := Parse(format, raw, true, 1024)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range doc.Fields {
				if field.Text == "PRIVATE" {
					field.Set("masked")
				}
			}
			raw, err = doc.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			var actual struct{ Tools []map[string]any }
			if err := json.Unmarshal(raw, &actual); err != nil {
				t.Fatal(err)
			}
			for _, tool := range actual.Tools {
				if nested, ok := tool["custom"].(map[string]any); ok {
					tool = nested
				}
				grammar := tool["format"].(map[string]any)["grammar"].(map[string]any)
				if tool["description"] != "masked" || tool["name"] != "PRIVATE_TOOL" || grammar["definition"] != "PRIVATE" {
					t.Fatalf("custom tool field selection: %s", raw)
				}
			}
		})
	}
}

func TestRejectAmbiguousCustomToolDefinitions(t *testing.T) {
	for _, tool := range []string{
		`{"type":"custom","custom":null}`,
		`{"type":"custom","custom":"PRIVATE"}`,
		`{"type":"custom","custom":{"description":42}}`,
		`{"type":"custom","custom":{"description":"PRIVATE"},"function":{}}`,
		`{"type":"function","custom":{"description":"PRIVATE"}}`,
		`{"type":"custom","function":{"description":"PRIVATE"}}`,
	} {
		body := []byte(`{"messages":[{"role":"user","content":"ok"}],"tools":[` + tool + `]}`)
		if _, err := Parse("openai", body, true, 1024); err == nil {
			t.Fatalf("ambiguous definition accepted: %s", tool)
		}
	}
}
