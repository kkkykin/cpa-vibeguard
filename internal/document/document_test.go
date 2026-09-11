package document

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONStrictAndNumbers(t *testing.T) {
	v, err := JSON([]byte(`{"id":9007199254740993,"nested":{"a":1},"empty":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if v.(map[string]any)["id"] != json.Number("9007199254740993") {
		t.Fatal("number lost precision")
	}
	for _, raw := range []string{`{"a":1,"a":2}`, `{"x":{"a":1,"a":2}}`, `{} {}`, `[1,]`, `null trailing`, `{"x":NaN}`, string([]byte{'"', 0xff, '"'}), strings.Repeat("[", MaxDepth+2) + strings.Repeat("]", MaxDepth+2)} {
		if _, err := JSON([]byte(raw)); err == nil {
			t.Errorf("accepted invalid input %q", raw)
		}
	}
}

func TestYAMLStrict(t *testing.T) {
	v, err := YAML([]byte("regex: '\\b\\d{2}\\b'\nnumber: 42\nflag: false\ntext: |-\n  hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	if v["regex"] != `\b\d{2}\b` || v["number"] != json.Number("42") || v["flag"] != false || v["text"] != "hello" {
		t.Fatal(v)
	}
	for _, raw := range []string{"a: 1\na: 2", "a: {b: 1, b: 2}", "a: 1\n---\nb: 2", "a: 1\n---", "[a, b]", "true: value", "a: &x x\nb: *x", "a: !!binary abc", "a: .nan"} {
		if _, err := YAML([]byte(raw)); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestPointer(t *testing.T) {
	v, _ := JSON([]byte(`{"a/b":{"~":["zero",null,"two"]},"":true}`))
	for _, tc := range []struct {
		pointer string
		value   any
		exists  bool
		fail    bool
	}{
		{"/a~1b/~0/0", "zero", true, false}, {"/a~1b/~0/1", nil, true, false}, {"/a~1b/~0/8", nil, false, false}, {"/missing", nil, false, false}, {"/", true, true, false},
		{"/a~1b/~0/01", nil, false, true}, {"/a~1b/~0/-", nil, false, true}, {"a", nil, false, true}, {"/~2", nil, false, true}, {"/a~1b/~0/0/x", nil, false, true},
	} {
		t.Run(tc.pointer, func(t *testing.T) {
			got, exists, err := Pointer(v, tc.pointer)
			if (err != nil) != tc.fail || exists != tc.exists || got != tc.value {
				t.Fatalf("got %v %v %v", got, exists, err)
			}
		})
	}
}

func TestErrorsDoNotEchoValues(t *testing.T) {
	for _, parse := range []func([]byte) (any, error){JSON, func(b []byte) (any, error) { return YAML(b) }} {
		_, err := parse([]byte(`{"secret":"DO-NOT-PRINT","oops":`))
		if err == nil || strings.Contains(err.Error(), "DO-NOT-PRINT") {
			t.Fatal(err)
		}
	}
}
