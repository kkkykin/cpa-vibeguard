package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesEchoedRequestTextUsesResponseMapping(t *testing.T) {
	doc, err := Parse("openai-response", []byte(`{"id":"routing-id","instructions":"PRIVATE","output":[],"tools":[{"type":"function","function":{"name":"PRIVATE_TOOL","description":"PRIVATE","parameters":{"type":"object","properties":{"PRIVATE_KEY":{"type":"string","description":"PRIVATE"}}}}}]}`), false, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Fields) != 3 {
		t.Fatalf("echoed fields: %+v", doc.Fields)
	}
	for _, field := range doc.Fields {
		if field.Text != "PRIVATE" {
			t.Fatal(field.Path)
		}
		field.Set("restored")
	}
	body, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "restored") != 3 || !strings.Contains(string(body), "PRIVATE_TOOL") || !strings.Contains(string(body), "PRIVATE_KEY") {
		t.Fatal(string(body))
	}
}

func TestResponsesSchemaSnapshotsAndValidation(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		root := map[string]any{"id": "r", "output": []any{}, "text": map[string]any{"format": map[string]any{"type": "json_schema", "description": "PRIVATE", "schema": map[string]any{"description": "PRIVATE", "items": map[string]any{"description": "PRIVATE"}}}}}
		doc, err := Snapshot(format, "response", root, 1024)
		if err != nil || len(doc.Fields) != 3 {
			t.Fatalf("snapshot schema: %v %+v", err, doc)
		}
		for _, field := range doc.Fields {
			field.Set("restored")
		}
		body, err := doc.Marshal()
		if err != nil || strings.Contains(string(body), "PRIVATE") || strings.Count(string(body), "restored") != 3 {
			t.Fatalf("snapshot schema output: %v %s", err, body)
		}
	}
	for _, text := range []any{"invalid", map[string]any{"format": "invalid"}, map[string]any{"format": map[string]any{"description": 42}}, map[string]any{"format": map[string]any{"schema": []any{}}}} {
		raw, _ := json.Marshal(map[string]any{"input": "ok", "text": text})
		if _, err := Parse("openai-response", raw, true, 1024); err == nil {
			t.Fatalf("invalid selected schema accepted: %s", raw)
		}
	}
}
