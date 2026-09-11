package protocol

import (
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
