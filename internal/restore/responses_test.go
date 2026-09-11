package restore

import (
	"bytes"
	"encoding/json"
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
