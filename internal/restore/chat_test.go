package restore

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
)

func chatToolEvent(choice int, calls ...map[string]any) map[string]any {
	items := make([]any, len(calls))
	for i, call := range calls {
		items[i] = call
	}
	return map[string]any{"choices": []any{map[string]any{
		"index": choice, "delta": map[string]any{"tool_calls": items},
	}}}
}

func TestChatCustomToolEveryMarkerSplit(t *testing.T) {
	for _, mode := range []string{"json", "sse"} {
		for _, initial := range []string{"input", "empty-input", "type-only"} {
			for split := 0; split <= len(marker); split++ {
				t.Run(fmt.Sprintf("%s/%s/%d", mode, initial, split), func(t *testing.T) {
					s := NewStream("openai", config.Defaults().Limits, lookup)
					var got strings.Builder
					push := func(event map[string]any) {
						t.Helper()
						out, _, err := s.Push(encodeEvent(event, mode))
						if err != nil {
							t.Fatal(err)
						}
						for _, event := range decodeEvents(t, out, mode) {
							got.WriteString(textAt(event, "choices", 0, "delta", "tool_calls", 0, "custom", "input"))
						}
					}
					first := map[string]any{"index": 0, "type": "custom", "id": "call-0"}
					if initial != "type-only" {
						input := ""
						if initial == "input" {
							input = marker[:split]
						}
						first["custom"] = map[string]any{"name": "run", "input": input}
					}
					push(chatToolEvent(0, first))
					if initial != "input" {
						push(chatToolEvent(0, map[string]any{"index": 0, "custom": map[string]any{"input": marker[:split]}}))
					}
					push(chatToolEvent(0, map[string]any{"index": 0, "custom": map[string]any{"input": marker[split:]}}))
					want, _ := lookup(marker)
					if got.String() != want {
						t.Fatalf("custom input not restored at marker completion: %q", got.String())
					}
					push(toolCases()[0].stop("", 0))
					if got.String() != want || s.hasPending() {
						t.Fatalf("finish duplicated input or retained a tail: %q", got.String())
					}
					s.Abort()
					if s.Buffered() != 0 {
						t.Fatal("complete retained tool state")
					}
				})
			}
		}
	}
}

func TestChatInterleavedToolKindsAndChoices(t *testing.T) {
	s := NewStream("openai", config.Defaults().Limits, lookup)
	type coordinate struct{ choice, tool int }
	got := make(map[coordinate]string)
	push := func(event map[string]any) {
		t.Helper()
		out, _, err := s.Push(encodeEvent(event, "json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range decodeEvents(t, out, "json") {
			for _, v := range event["choices"].([]any) {
				choice := v.(map[string]any)
				i, _ := index(choice, "index", 0)
				delta := choice["delta"].(map[string]any)
				calls, _ := delta["tool_calls"].([]any)
				for _, v := range calls {
					call := v.(map[string]any)
					j, _ := index(call, "index", 0)
					got[coordinate{i, j}] += textAt(call, "custom", "input") + textAt(call, "function", "arguments")
				}
			}
		}
	}
	for _, end := range []bool{false, true} {
		for _, i := range []int{1, 0} {
			var calls []map[string]any
			for _, j := range []int{1, 0} {
				call := map[string]any{"index": j}
				if i == j {
					input := marker[:30]
					if end {
						input = marker[30:]
					} else {
						call["type"] = "custom"
					}
					call["custom"] = map[string]any{"input": input}
				} else {
					// Typeless function deltas remain valid.
					args := `{"q":"` + marker[:30]
					if end {
						args = marker[30:] + `"}`
					}
					call["function"] = map[string]any{"arguments": args}
				}
				calls = append(calls, call)
			}
			push(chatToolEvent(i, calls...))
		}
	}
	for _, i := range []int{0, 1} {
		push(map[string]any{"choices": []any{map[string]any{"index": i, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})
	}
	want, _ := lookup(marker)
	for i := 0; i < 2; i++ {
		for j := 0; j < 2; j++ {
			value := got[coordinate{i, j}]
			if i != j {
				var args struct{ Q string }
				if err := json.Unmarshal([]byte(value), &args); err != nil {
					t.Fatalf("function arguments corrupted: %v %q", err, value)
				}
				value = args.Q
			}
			if value != want {
				t.Fatalf("choice %d tool %d: %q", i, j, value)
			}
		}
	}
}

func TestChatRejectsConflictingToolKinds(t *testing.T) {
	for _, tc := range []struct {
		name, first, next string
	}{
		{"custom-to-function-metadata", `{"type":"custom"}`, `{"type":"function"}`},
		{"function-to-custom-metadata", `{"type":"function"}`, `{"type":"custom"}`},
		{"custom-to-function-wrapper", `{"type":"custom"}`, `{"function":{"arguments":"{}"}}`},
		{"function-to-custom-wrapper", `{"function":{"arguments":""}}`, `{"custom":{"input":"text"}}`},
		{"custom-mismatched-wrapper", `{}`, `{"type":"custom","function":{"arguments":"{}"}}`},
		{"function-mismatched-wrapper", `{}`, `{"type":"function","custom":{"input":"text"}}`},
		{"two-wrappers", `{}`, `{"custom":{"input":"text"},"function":{"arguments":"{}"}}`},
		{"unknown-kind", `{}`, `{"type":"future","custom":{"input":"text"}}`},
		{"invalid-kind", `{}`, `{"type":42}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStream("openai", config.Defaults().Limits, lookup)
			var first, next map[string]any
			json.Unmarshal([]byte(tc.first), &first)
			json.Unmarshal([]byte(tc.next), &next)
			if _, _, err := s.Push(encodeEvent(chatToolEvent(0, first), "json")); err != nil {
				t.Fatal(err)
			}
			if out, _, err := s.Push(encodeEvent(chatToolEvent(0, next), "json")); err == nil || len(out) != 0 {
				t.Fatalf("ambiguous tool type accepted: %v %s", err, out)
			}
			s.Abort()
			if s.Buffered() != 0 {
				t.Fatal("abort retained tool metadata")
			}
		})
	}
}

func TestChatToolMetadataLimitsAndCleanup(t *testing.T) {
	metadata := func(i int) []byte {
		return encodeEvent(chatToolEvent(0, map[string]any{"index": i, "type": "custom"}), "json")
	}
	s := NewStream("openai", config.Defaults().Limits, lookup)
	for i := 0; i < MaxStreamFields; i++ {
		if _, _, err := s.Push(metadata(i)); err != nil {
			t.Fatalf("metadata %d: %v", i, err)
		}
	}
	if s.Buffered() < MaxStreamFields*32 {
		t.Fatal("tool metadata missing from state accounting")
	}
	if _, _, err := s.Push(metadata(MaxStreamFields)); err == nil {
		t.Fatal("tool metadata count is unbounded")
	}
	s.Abort()
	if s.Buffered() != 0 {
		t.Fatal("abort retained metadata")
	}
	l := config.Defaults().Limits
	l.RequestStateBytes = 512
	s = NewStream("openai", l, lookup)
	limited := false
	for i := 0; i < 16; i++ {
		if _, _, err := s.Push(metadata(i)); err != nil {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("tool metadata bypassed request state budget")
	}
	s.Abort()
	if s.Buffered() != 0 {
		t.Fatal("state limit cleanup retained metadata")
	}
}

func TestChatCustomToolNormalTailAndAbort(t *testing.T) {
	for _, normal := range []bool{true, false} {
		s := NewStream("openai", config.Defaults().Limits, lookup)
		first := chatToolEvent(0, map[string]any{"type": "custom", "custom": map[string]any{"input": marker[:30]}})
		out, _, err := s.Push(encodeEvent(first, "json"))
		if err != nil || textAt(decodeEvents(t, out, "json")[0], "choices", 0, "delta", "tool_calls", 0, "custom", "input") != "" {
			t.Fatalf("partial marker escaped: %v %s", err, out)
		}
		if normal {
			out, _, err = s.Push(encodeEvent(toolCases()[0].stop("", 0), "json"))
			if err != nil || textAt(decodeEvents(t, out, "json")[0], "choices", 0, "delta", "tool_calls", 0, "custom", "input") != marker[:30] {
				t.Fatalf("normal end lost partial input: %v %s", err, out)
			}
			if _, _, err := s.Push(encodeEvent(chatToolEvent(0, map[string]any{"type": "custom"}), "json")); err == nil {
				t.Fatal("tool metadata accepted after choice finish")
			}
		}
		s.Abort()
		if s.Buffered() != 0 {
			t.Fatal("cleanup retained tail or tool metadata")
		}
	}
}
