package restore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
)

func TestResponsesTailSequencesAndNativeJSONFraming(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		for _, mode := range []string{"sse", "json"} {
			t.Run(format+"/"+mode, func(t *testing.T) {
				s := NewStream(format, config.Defaults().Limits, lookup)
				tc := textCases()[3]
				inputs := []map[string]any{tc.delta(marker[:13], 0), tc.delta(marker[:30], 1), tc.stop(marker[:13], 0), tc.stop(marker[:30], 1), {"type": "response.completed", "response": map[string]any{"id": "r", "output": []any{}}}}
				const base = int64(1<<53 + 2)
				var events []map[string]any
				for i, input := range inputs {
					input["sequence_number"] = base + int64(i)
					out, _, err := s.Push(encodeEvent(input, mode))
					if err != nil {
						t.Fatal(err)
					}
					outputMode := mode
					if bytes.HasPrefix(out, []byte("data:")) {
						outputMode = "sse"
					}
					events = append(events, decodeEvents(t, out, outputMode)...)
				}
				if len(events) != 7 {
					t.Fatalf("tail deltas missing: %+v", events)
				}
				for i, event := range events {
					sequence, err := event["sequence_number"].(json.Number).Int64()
					if err != nil || sequence != base+int64(i) {
						t.Fatalf("sequence at %d: %v", i, event)
					}
				}
				if joined(tc, events) != marker[:13]+marker[:30] || events[2]["type"] != "response.output_text.delta" || events[3]["type"] != "response.output_text.done" || events[4]["type"] != "response.output_text.delta" || events[5]["type"] != "response.output_text.done" {
					t.Fatalf("tail/terminal ordering: %+v", events)
				}
			})
		}
	}
}

func TestResponsesSequenceOverflowRejectsWithoutOutput(t *testing.T) {
	s := NewStream("openai-response", config.Defaults().Limits, lookup)
	tc := textCases()[3]
	if _, _, err := s.Push(encodeEvent(tc.delta(marker[:20], 0), "sse")); err != nil {
		t.Fatal(err)
	}
	stop := tc.stop(marker[:20], 0)
	stop["sequence_number"] = int64(1<<63 - 1)
	if out, _, err := s.Push(encodeEvent(stop, "sse")); err == nil || out != nil {
		t.Fatal("tail insertion overflowed the sequence counter")
	}
}

func TestResponsesMCPEmptyStartAndEveryArgumentSplit(t *testing.T) {
	for _, format := range []string{"openai-response", "codex"} {
		for _, mode := range []string{"sse", "json"} {
			t.Run(format+"/"+mode, func(t *testing.T) {
				for split := 0; split <= len(marker); split++ {
					s := NewStream(format, config.Defaults().Limits, lookup)
					arguments := `{"value":"` + marker + `"}`
					item := func(args string) map[string]any {
						return map[string]any{"type": "mcp_call", "id": "mcp-1", "name": "lookup", "server_label": "server", "arguments": args, "output": marker}
					}
					inputs := []map[string]any{
						{"type": "response.output_item.added", "output_index": 0, "item": item("")},
						{"type": "response.mcp_call_arguments.delta", "output_index": 0, "item_id": "mcp-1", "delta": `{"value":"` + marker[:split]},
						{"type": "response.mcp_call_arguments.delta", "output_index": 0, "item_id": "mcp-1", "delta": marker[split:] + `"}`},
						{"type": "response.mcp_call_arguments.done", "output_index": 0, "item_id": "mcp-1", "arguments": arguments},
						{"type": "response.output_item.done", "output_index": 0, "item": item(arguments)},
						{"type": "response.completed", "response": map[string]any{"id": "r", "output": []any{item(arguments)}}},
					}
					var deltas strings.Builder
					secret, _ := lookup(marker)
					for i, input := range inputs {
						out, _, err := s.Push(encodeEvent(input, mode))
						if err != nil {
							t.Fatalf("split %d event %d: %v", split, i, err)
						}
						for _, event := range decodeEvents(t, out, mode) {
							if eventName(event) == "response.mcp_call_arguments.delta" {
								deltas.WriteString(textAt(event, "delta"))
							}
							if i == 0 && (textAt(event, "item", "arguments") != "" || textAt(event, "item", "output") != secret) {
								t.Fatalf("partial item lost fields: %v", event)
							}
						}
						if i >= 3 && bytes.Contains(out, []byte(marker)) {
							t.Fatalf("completed snapshot not restored: %s", out)
						}
					}
					var args struct{ Value string }
					if json.Unmarshal([]byte(deltas.String()), &args) != nil || args.Value != secret || !s.ended {
						t.Fatalf("split %d: invalid restored arguments %q", split, deltas.String())
					}
				}
			})
		}
	}
}

func TestResponsesMCPStillRejectsInvalidCompletedArguments(t *testing.T) {
	for _, args := range []string{"", "{", `{"key":1,"key":2}`} {
		for _, added := range []bool{false, true} {
			if added && args == "" {
				continue
			}
			t.Run(fmt.Sprintf("added=%t/args=%q", added, args), func(t *testing.T) {
				s := NewStream("openai-response", config.Defaults().Limits, lookup)
				item := map[string]any{"type": "mcp_call", "arguments": args}
				if err := s.responseItem(item, added); err == nil {
					t.Fatal("invalid nonempty/complete arguments accepted")
				}
			})
		}
	}
}
