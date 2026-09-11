package protocol

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/document"
)

type fixture struct {
	Name, Format      string
	Request, Response json.RawMessage
	RequestFields     []string `json:"request_fields"`
	ResponseFields    []string `json:"response_fields"`
}

func fixtures(t *testing.T) []fixture {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/protocols.json")
	if err != nil {
		t.Fatal(err)
	}
	var fs []fixture
	if err := json.Unmarshal(raw, &fs); err != nil {
		t.Fatal(err)
	}
	return fs
}
func TestProtocolFixtures(t *testing.T) {
	for _, f := range fixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			for _, request := range []bool{true, false} {
				raw, expected := f.Response, f.ResponseFields
				if request {
					raw, expected = f.Request, f.RequestFields
				}
				d, err := Parse(f.Format, raw, request, 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				paths := make([]string, 0, len(d.Fields))
				for _, field := range d.Fields {
					paths = append(paths, field.Path)
					if field.Text != "PRIVATE" {
						t.Fatalf("selected an opaque field %s = %q", field.Path, field.Text)
					}
					field.Set("MASKED")
				}
				sort.Strings(paths)
				sort.Strings(expected)
				if !reflect.DeepEqual(paths, expected) {
					t.Fatalf("request=%v: fields=%v expected=%v", request, paths, expected)
				}
				masked, err := d.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				roundtrip, err := Parse(f.Format, masked, request, 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				for _, field := range roundtrip.Fields {
					if field.Text != "MASKED" {
						t.Fatalf("field not rewritten %s", field.Path)
					}
					field.Set("PRIVATE")
				}
				restored, err := roundtrip.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				want, err := Parse(f.Format, raw, request, 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				normalized, _ := want.Marshal()
				a, _ := document.JSON(restored)
				b, _ := document.JSON(normalized)
				if !reflect.DeepEqual(a, b) {
					t.Fatalf("round trip changed protocol identifiers or opaque data: %s", restored)
				}
			}
		})
	}
}

func TestRejectInvalidKnownStructures(t *testing.T) {
	for _, tc := range []struct{ format, raw string }{
		{"openai", `{"messages":[{"role":"user","content":{"text":"PRIVATE"}}]}`},
		{"openai", `{"messages":[{"role":"user","content":[{"type":"future","text":"PRIVATE"}]}]}`},
		{"openai", `{"messages":[],"messages":[]}`},
		{"openai", `{"messages":[{"role":"user","content":"x"}],"input":"x"}`},
		{"openai-response", `{"input":[{"type":"future","content":"PRIVATE"}]}`},
		{"openai-response", `{"input":[{"type":"function_call","arguments":"{\"a\":1,\"a\":2}"}]}`},
		{"openai-response", `{"input":[],"messages":[]}`},
		{"claude", `{"messages":[{"role":"user","content":[{"type":"future","text":"PRIVATE"}]}]}`},
		{"gemini", `{"contents":[{"parts":[{"future":"PRIVATE"}]}]}`},
		{"gemini", `{"contents":[{"parts":[{"text":"PRIVATE","functionCall":{"args":{}}}]}]}`},
		{"interactions", `{"input":[{"type":"future","content":"PRIVATE"}]}`},
		{"interactions", `{"input":"PRIVATE","instructions":"PRIVATE"}`},
		{"antigravity", `{"contents":[{"parts":[{"text":"PRIVATE"}]}]}`},
	} {
		t.Run(tc.format+tc.raw, func(t *testing.T) {
			if _, err := Parse(tc.format, []byte(tc.raw), true, 1024); err == nil || err == ErrUnknown {
				t.Fatalf("known malformed body must be a processing error: %v", err)
			}
		})
	}
	for _, format := range []string{"", "OPENAI", "future"} {
		if _, err := Parse(format, []byte(`{"messages":[]}`), true, 1024); err != ErrUnknown {
			t.Fatal(err)
		}
	}
}

func TestToolJSONEscapingAndLimit(t *testing.T) {
	raw := []byte(`{"input":[{"type":"function_call","arguments":"{\"text\":\"MASKED\",\"array\":[\"MASKED\"],\"number\":9007199254740993}"}]}`)
	d, err := Parse("openai-response", raw, true, 1024)
	if err != nil {
		t.Fatal(err)
	}
	original := "中文\"\n\\path"
	for _, field := range d.Fields {
		field.Set(original)
	}
	b, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	d, err = Parse("openai-response", b, true, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range d.Fields {
		if field.Text != original {
			t.Fatal(field.Text)
		}
	}
	if !strings.Contains(string(b), "9007199254740993") {
		t.Fatal("number rounded")
	}
	if _, err := Parse("openai-response", raw, true, 4); err == nil {
		t.Fatal("tool JSON limit ignored")
	}
	for _, field := range d.Fields {
		field.Set(strings.Repeat("x", 1024))
	}
	if _, err := d.Marshal(); err == nil {
		t.Fatal("expanded tool JSON limit ignored")
	}
}

func TestHintIsStableAndDoesNotOverwriteInstructions(t *testing.T) {
	for _, f := range fixtures(t) {
		if f.Name == "legacy-completions" || f.Format == "openai-image" || f.Format == "openai-video" || f.Format == "codex-alpha-search" {
			continue
		}
		d, err := Parse(f.Format, f.Request, true, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		d.AppendHint()
		one, err := d.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		d.AppendHint()
		two, _ := d.Marshal()
		if string(one) != string(two) || strings.Count(string(one), "Preserve privacy markers exactly as written") != 1 {
			t.Fatalf("hint not idempotent: %s", one)
		}
		if !strings.Contains(string(one), "PRIVATE") {
			t.Fatal("instructions overwritten")
		}
	}
}
