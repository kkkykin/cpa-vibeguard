package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestChatPredictionValidation(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{`null`, true},
		{`{"type":"content","content":"PRIVATE"}`, true},
		{`{"type":"content","content":[{"type":"text","text":"PRIVATE"}]}`, true},
		{`"PRIVATE"`, false},
		{`[]`, false},
		{`{"content":"PRIVATE"}`, false},
		{`{"type":"future","content":"PRIVATE"}`, false},
		{`{"type":42,"content":"PRIVATE"}`, false},
		{`{"type":"content"}`, false},
		{`{"type":"content","content":null}`, false},
		{`{"type":"content","content":42}`, false},
		{`{"type":"content","content":{"text":"PRIVATE"}}`, false},
		{`{"type":"content","content":["PRIVATE"]}`, false},
		{`{"type":"content","content":[{"type":"future","text":"PRIVATE"}]}`, false},
		{`{"type":"content","content":[{"type":"image_url","image_url":{"url":"PRIVATE"}}]}`, false},
		{`{"type":"content","content":[{"type":"text"}]}`, false},
		{`{"type":"content","content":[{"type":"text","text":null}]}`, false},
		{`{"type":"content","content":[{"type":"text","text":42}]}`, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			raw := []byte(`{"messages":[{"role":"user","content":"ok"}],"prediction":` + tc.value + `}`)
			doc, err := Parse("openai", raw, true, 1024)
			if !tc.valid {
				if err == nil || err == ErrUnknown {
					t.Fatalf("malformed prediction accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range doc.Fields {
				if field.Text == "PRIVATE" {
					field.Set("MASKED")
				}
			}
			out, err := doc.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			json.Unmarshal(out, &got)
			json.Unmarshal([]byte(strings.ReplaceAll(string(raw), "PRIVATE", "MASKED")), &want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("prediction was not rewritten or null changed: %s", out)
			}
		})
	}
}

func TestSchemaSubschemaDescriptions(t *testing.T) {
	for _, key := range []string{"additionalItems", "unevaluatedItems", "unevaluatedProperties", "propertyNames", "contentSchema"} {
		t.Run(key, func(t *testing.T) {
			for _, value := range []string{`{"description":"PRIVATE","title":"PRIVATE","pattern":"PRIVATE","default":"PRIVATE","const":"PRIVATE","enum":["PRIVATE"]}`, `true`, `false`, `null`, `"PRIVATE"`, `[]`} {
				raw := []byte(`{"input":"ok","text":{"format":{"type":"json_schema","schema":{"` + key + `":` + value + `}}}}`)
				doc, err := Parse("openai-response", raw, true, 1024)
				if value == `null` || value == `"PRIVATE"` || value == `[]` {
					if err == nil {
						t.Fatalf("invalid %s schema accepted: %s", key, value)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if value == `true` || value == `false` {
					if len(doc.Fields) != 1 {
						t.Fatalf("boolean schema selected as text: %+v", doc.Fields)
					}
					continue
				}
				want := []string{"/input", "/text/format/schema/" + key + "/description", "/text/format/schema/" + key + "/title"}
				var paths []string
				for _, field := range doc.Fields {
					paths = append(paths, field.Path)
				}
				if !reflect.DeepEqual(paths, want) {
					t.Fatalf("wrong schema fields: %v", paths)
				}
			}
		})
	}
}

func TestDependenciesValidateSchemasAndPreservePropertyNames(t *testing.T) {
	for _, value := range []string{`null`, `[]`, `"PRIVATE"`, `{"key":null}`, `{"key":42}`, `{"key":["PRIVATE",{}]}`} {
		raw := []byte(`{"input":"ok","tools":[{"type":"function","parameters":{"dependencies":` + value + `}}]}`)
		if _, err := Parse("openai-response", raw, true, 1024); err == nil {
			t.Fatalf("invalid dependencies accepted: %s", value)
		}
	}
	for _, key := range []string{"dependencies", "dependentSchemas"} {
		raw := []byte(`{"input":"ok","tools":[{"type":"function","parameters":{"` + key + `":{"key":true,"other":false}}}]}`)
		if _, err := Parse("openai-response", raw, true, 1024); err != nil {
			t.Fatal(err)
		}
	}
	raw := []byte(`{"input":"ok","tools":[{"type":"function","parameters":{"dependencies":{"PRIVATE":["PRIVATE"]}}}]}`)
	doc, err := Parse("openai-response", raw, true, 1024)
	if err != nil || len(doc.Fields) != 1 {
		t.Fatalf("property dependencies treated as text: %v %+v", err, doc)
	}
}

func TestMCPDescriptionSnapshots(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		root := map[string]any{"id": "r", "output": []any{}, "tools": []any{map[string]any{
			"type": "mcp", "server_description": "PRIVATE", "server_label": "PRIVATE", "server_url": "PRIVATE",
		}}}
		doc, err := Snapshot(format, "response", root, 1024)
		if err != nil || len(doc.Fields) != 1 || doc.Fields[0].Path != "/tools/0/server_description" {
			t.Fatalf("MCP snapshot fields: %v %+v", err, doc)
		}
		doc.Fields[0].Set("restored")
		tool := root["tools"].([]any)[0].(map[string]any)
		if tool["server_description"] != "restored" || tool["server_label"] != "PRIVATE" || tool["server_url"] != "PRIVATE" {
			t.Fatalf("MCP snapshot changed identifiers: %+v", tool)
		}
	}
	if _, err := Parse("openai-response", []byte(`{"input":"ok","tools":[{"type":"mcp","server_description":42}]}`), true, 1024); err == nil {
		t.Fatal("invalid MCP description accepted")
	}
}

func TestGeminiFunctionResponseSchemaValidation(t *testing.T) {
	for _, format := range []string{"gemini", "antigravity"} {
		for _, key := range []string{"response", "responseJsonSchema"} {
			for _, value := range []string{`{"description":"PRIVATE"}`, `{"description":42}`, `"PRIVATE"`, `null`} {
				raw := `{"contents":[{"parts":[{"text":"ok"}]}],"tools":[{"functionDeclarations":[{"name":"run","` + key + `":` + value + `}]}]}`
				if format == "antigravity" {
					raw = `{"request":` + raw + `}`
				}
				doc, err := Parse(format, []byte(raw), true, 1024)
				if value != `{"description":"PRIVATE"}` {
					if err == nil {
						t.Fatalf("invalid %s %s accepted: %s", format, key, value)
					}
				} else if err != nil || len(doc.Fields) != 2 || doc.Fields[1].Text != "PRIVATE" {
					t.Fatalf("response schema not selected: %v %+v", err, doc)
				}
			}
		}
	}
}
