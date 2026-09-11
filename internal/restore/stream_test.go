package restore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
	"github.com/kkkykin/cpa-vibeguard/internal/document"
)

type streamCase struct {
	format, mode string
	delta        func(string, int) map[string]any
	stop         func(string, int) map[string]any
	value        func(map[string]any) string
}

func nested(m map[string]any, path ...any) any {
	var v any = m
	for _, p := range path {
		switch k := p.(type) {
		case string:
			n, _ := v.(map[string]any)
			v = n[k]
		case int:
			n, _ := v.([]any)
			if k >= len(n) {
				return nil
			}
			v = n[k]
		}
	}
	return v
}
func textAt(m map[string]any, path ...any) string { s, _ := nested(m, path...).(string); return s }
func textCases() []streamCase {
	chat := streamCase{"openai", "json",
		func(v string, i int) map[string]any {
			return map[string]any{"choices": []any{map[string]any{"index": i, "delta": map[string]any{"content": v, "role": "assistant"}, "finish_reason": nil}}, "usage": map[string]any{"total_tokens": 7}}
		},
		func(v string, i int) map[string]any {
			return map[string]any{"choices": []any{map[string]any{"index": i, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"total_tokens": 7}}
		},
		func(m map[string]any) string { return textAt(m, "choices", 0, "delta", "content") }}
	gemini := streamCase{"gemini", "json",
		func(v string, i int) map[string]any {
			return map[string]any{"candidates": []any{map[string]any{"index": i, "content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": v, "thoughtSignature": marker}}}}}}
		},
		func(v string, i int) map[string]any {
			return map[string]any{"candidates": []any{map[string]any{"index": i, "finishReason": "STOP"}}, "usageMetadata": map[string]any{"totalTokenCount": 7}}
		},
		func(m map[string]any) string { return textAt(m, "candidates", 0, "content", "parts", 0, "text") }}
	claude := streamCase{"claude", "sse",
		func(v string, i int) map[string]any {
			return map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": v}}
		},
		func(v string, i int) map[string]any { return map[string]any{"type": "content_block_stop", "index": i} },
		func(m map[string]any) string { return textAt(m, "delta", "text") }}
	responses := streamCase{"openai-response", "sse",
		func(v string, i int) map[string]any {
			return map[string]any{"type": "response.output_text.delta", "output_index": i, "item_id": fmt.Sprintf("msg_%d", i), "content_index": 2, "delta": v}
		},
		func(v string, i int) map[string]any {
			return map[string]any{"type": "response.output_text.done", "output_index": i, "item_id": fmt.Sprintf("msg_%d", i), "content_index": 2, "text": v}
		},
		func(m map[string]any) string {
			if eventName(m) == "response.output_text.delta" {
				return textAt(m, "delta")
			}
			return ""
		}}
	interactions := streamCase{"interactions", "sse",
		func(v string, i int) map[string]any {
			return map[string]any{"event_type": "step.delta", "index": i, "delta": map[string]any{"type": "text", "text": v}}
		},
		func(v string, i int) map[string]any { return map[string]any{"event_type": "step.stop", "index": i} },
		func(m map[string]any) string { return textAt(m, "delta", "text") }}
	return []streamCase{chat, gemini, claude, responses, interactions}
}
func encodeEvent(m map[string]any, mode string) []byte {
	b, _ := json.Marshal(m)
	if mode == "sse" {
		return append(append([]byte("event: "+eventName(m)+"\ndata: "), b...), '\n', '\n')
	}
	return b
}
func decodeEvents(t *testing.T, b []byte, mode string) []map[string]any {
	t.Helper()
	if len(b) == 0 {
		return nil
	}
	var out []map[string]any
	if mode == "sse" {
		for _, frame := range bytes.Split(b, []byte("\n\n")) {
			if len(frame) == 0 {
				continue
			}
			var lines [][]byte
			for _, line := range bytes.Split(frame, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("data:")) {
					lines = append(lines, bytes.TrimSpace(line[5:]))
				}
			}
			if len(lines) == 0 {
				continue
			}
			v, err := document.JSON(bytes.Join(lines, []byte("\n")))
			if err != nil {
				t.Fatalf("invalid SSE output: %v %s", err, b)
			}
			out = append(out, v.(map[string]any))
		}
	} else {
		v, err := document.JSON(b)
		if err != nil {
			t.Fatalf("invalid JSON output: %v %s", err, b)
		}
		if mode == "array" {
			for _, v := range v.([]any) {
				out = append(out, v.(map[string]any))
			}
		} else {
			out = append(out, v.(map[string]any))
		}
	}
	return out
}
func pushEvent(t *testing.T, s *Stream, tc streamCase, m map[string]any) []map[string]any {
	t.Helper()
	out, _, err := s.Push(encodeEvent(m, tc.mode))
	if err != nil {
		t.Fatalf("%s: %v; input=%v", tc.format, err, m)
	}
	return decodeEvents(t, out, tc.mode)
}
func joined(tc streamCase, events []map[string]any) string {
	var b strings.Builder
	for _, m := range events {
		b.WriteString(tc.value(m))
	}
	return b.String()
}

func TestProtocolsEveryMarkerSplit(t *testing.T) {
	for _, tc := range textCases() {
		t.Run(tc.format, func(t *testing.T) {
			for split := 0; split <= len(marker); split++ {
				s := NewStream(tc.format, config.Defaults().Limits, lookup)
				events := pushEvent(t, s, tc, tc.delta("before "+marker[:split], 0))
				for _, f := range s.fields {
					if f.text != nil && f.text.Buffered() > 47 {
						t.Fatal("unbounded text tail")
					}
				}
				events = append(events, pushEvent(t, s, tc, tc.delta(marker[split:]+" after", 0))...)
				if got := joined(tc, events); got != "before 中文\"\n\\secret after" {
					t.Fatalf("split %d: %q", split, got)
				}
				events = append(events, pushEvent(t, s, tc, tc.stop("before "+marker+" after", 0))...)
				if got := joined(tc, events); got != "before 中文\"\n\\secret after" {
					t.Fatalf("snapshot duplicated delta at %d: %q", split, got)
				}
			}
		})
	}
}
func TestProtocolsNormalAndAbnormalEnds(t *testing.T) {
	for _, tc := range textCases() {
		t.Run(tc.format, func(t *testing.T) {
			for split := 1; split < len(marker); split++ {
				s := NewStream(tc.format, config.Defaults().Limits, lookup)
				events := pushEvent(t, s, tc, tc.delta("delivered "+marker[:split], 0))
				if joined(tc, events) != "delivered " {
					t.Fatal("partial tail escaped")
				}
				events = append(events, pushEvent(t, s, tc, tc.stop("delivered "+marker[:split], 0))...)
				if got := joined(tc, events); got != "delivered "+marker[:split] {
					t.Fatalf("normal tail %d: %q", split, got)
				}
				s = NewStream(tc.format, config.Defaults().Limits, lookup)
				events = pushEvent(t, s, tc, tc.delta("delivered "+marker[:split], 0))
				s.Abort()
				if s.Buffered() != 0 || joined(tc, events) != "delivered " {
					t.Fatal("abort replayed or retained state")
				}
			}
		})
	}
}
func TestProtocolsInterleavedOutputs(t *testing.T) {
	for _, tc := range textCases() {
		t.Run(tc.format, func(t *testing.T) {
			s := NewStream(tc.format, config.Defaults().Limits, lookup)
			if got := joined(tc, pushEvent(t, s, tc, tc.delta(marker[:24], 0))); got != "" {
				t.Fatal(got)
			}
			if got := joined(tc, pushEvent(t, s, tc, tc.delta("other "+marker, 1))); got != "other 中文\"\n\\secret" {
				t.Fatal(got)
			}
			if got := joined(tc, pushEvent(t, s, tc, tc.delta(marker[24:], 0))); got != "中文\"\n\\secret" {
				t.Fatal(got)
			}
			pushEvent(t, s, tc, tc.stop(marker, 0))
			pushEvent(t, s, tc, tc.stop(marker, 1))
		})
	}
}
func toolCases() []streamCase {
	chat := streamCase{"openai", "json",
		func(v string, i int) map[string]any {
			return map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": v, "name": "lookup"}}}}}}}
		},
		func(v string, i int) map[string]any {
			return map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}}
		},
		func(m map[string]any) string {
			return textAt(m, "choices", 0, "delta", "tool_calls", 0, "function", "arguments")
		}}
	claude := streamCase{"claude", "sse",
		func(v string, i int) map[string]any {
			return map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": v}}
		},
		func(v string, i int) map[string]any { return map[string]any{"type": "content_block_stop", "index": i} },
		func(m map[string]any) string { return textAt(m, "delta", "partial_json") }}
	responses := streamCase{"openai-response", "sse",
		func(v string, i int) map[string]any {
			return map[string]any{"type": "response.function_call_arguments.delta", "item_id": fmt.Sprintf("tool_%d", i), "output_index": i, "delta": v}
		},
		func(v string, i int) map[string]any {
			return map[string]any{"type": "response.function_call_arguments.done", "item_id": fmt.Sprintf("tool_%d", i), "output_index": i, "arguments": v}
		},
		func(m map[string]any) string {
			if eventName(m) == "response.function_call_arguments.delta" {
				return textAt(m, "delta")
			}
			return ""
		}}
	interactions := streamCase{"interactions", "sse",
		func(v string, i int) map[string]any {
			return map[string]any{"event_type": "step.delta", "index": i, "delta": map[string]any{"type": "arguments_delta", "arguments": v}}
		},
		func(v string, i int) map[string]any { return map[string]any{"event_type": "step.stop", "index": i} },
		func(m map[string]any) string { return textAt(m, "delta", "arguments") }}
	return []streamCase{chat, claude, responses, interactions}
}
func TestProtocolsToolJSONEverySplit(t *testing.T) {
	argument := `{"` + marker + `":"` + marker + `","nested":["中文","\ud83d\ude00"],"n":9007199254740993}`
	for _, tc := range toolCases() {
		t.Run(tc.format, func(t *testing.T) {
			for split := 0; split <= len(argument); split++ {
				s := NewStream(tc.format, config.Defaults().Limits, lookup)
				events := pushEvent(t, s, tc, tc.delta(argument[:split], 0))
				events = append(events, pushEvent(t, s, tc, tc.delta(argument[split:], 0))...)
				events = append(events, pushEvent(t, s, tc, tc.stop(argument, 0))...)
				got := joined(tc, events)
				v, err := document.JSON([]byte(got))
				if err != nil {
					t.Fatalf("split %d: %v %s", split, err, got)
				}
				if v.(map[string]any)[marker] != "中文\"\n\\secret" {
					t.Fatalf("split %d changed key or missed value: %s", split, got)
				}
				if !strings.Contains(got, "9007199254740993") {
					t.Fatal("number rounded")
				}
			}
			// A complete marker cannot wait for the tool JSON closing quote.
			s := NewStream(tc.format, config.Defaults().Limits, lookup)
			got := joined(tc, pushEvent(t, s, tc, tc.delta(`{"q":"`+marker, 0)))
			if !strings.Contains(got, "secret") {
				t.Fatal("complete tool marker was buffered")
			}
			if _, _, err := s.Push(encodeEvent(tc.stop("", 0), tc.mode)); err == nil {
				t.Fatal("accepted incomplete tool JSON at normal stop")
			}
		})
	}
}
func TestParallelFieldsKeepControlAndOpaqueData(t *testing.T) {
	s := NewStream("openai", config.Defaults().Limits, lookup)
	m := map[string]any{"id": marker, "choices": []any{
		map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": marker[:30]}},
		map[string]any{"index": 1, "delta": map[string]any{"content": marker}}}, "usage": map[string]any{"total_tokens": 9}}
	b, _, err := s.Push(encodeEvent(m, "json"))
	if err != nil {
		t.Fatal(err)
	}
	o := decodeEvents(t, b, "json")[0]
	if textAt(o, "choices", 0, "delta", "content") != "" || textAt(o, "choices", 0, "delta", "role") != "assistant" || !strings.Contains(textAt(o, "choices", 1, "delta", "content"), "secret") || textAt(o, "id") != marker || nested(o, "usage", "total_tokens") != json.Number("9") {
		t.Fatal(string(b))
	}
	// Gemini object tool arguments are snapshots, including repeated snapshots.
	s = NewStream("gemini", config.Defaults().Limits, lookup)
	m = map[string]any{"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"parts": []any{map[string]any{"functionCall": map[string]any{"name": marker, "args": map[string]any{marker: marker}}, "thoughtSignature": marker}}}}}}
	for i := 0; i < 2; i++ {
		b, _, err = s.Push(encodeEvent(m, "json"))
		if err != nil {
			t.Fatal(err)
		}
		o = decodeEvents(t, b, "json")[0]
		if textAt(o, "candidates", 0, "content", "parts", 0, "functionCall", "name") != marker || textAt(o, "candidates", 0, "content", "parts", 0, "thoughtSignature") != marker || !strings.Contains(textAt(o, "candidates", 0, "content", "parts", 0, "functionCall", "args", marker), "secret") {
			t.Fatal(string(b))
		}
	}
}
func TestResponsesSnapshotsAliasesAndCompletion(t *testing.T) {
	s := NewStream("openai-response", config.Defaults().Limits, lookup)
	tc := textCases()[3]
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_1", "status": "in_progress"}},
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_0", "type": "message", "role": "assistant", "content": []any{}}},
		tc.delta(marker[:30], 0),
		{"type": "response.output_text.delta", "item_id": "msg_0", "content_index": 2, "delta": marker[30:]},
		tc.stop(marker, 0),
		{"type": "response.completed", "response": map[string]any{"id": "resp_1", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": marker}}}}}},
	}
	var collected []map[string]any
	for _, m := range events {
		collected = append(collected, pushEvent(t, s, tc, m)...)
	}
	if joined(tc, collected) != "中文\"\n\\secret" {
		t.Fatal("delta/snapshot duplicated")
	}
	last := collected[len(collected)-1]
	if !strings.Contains(textAt(last, "response", "output", 0, "content", 0, "text"), "secret") {
		t.Fatal("completion snapshot not restored")
	}
}
func TestProtocolFramingSplitsAndArray(t *testing.T) {
	for _, tc := range textCases() {
		tc.mode = "sse"
		raw := append(encodeEvent(tc.delta(marker[:20], 0), "sse"), encodeEvent(tc.delta(marker[20:], 0), "sse")...)
		raw = append(raw, encodeEvent(tc.stop(marker, 0), "sse")...)
		for split := 0; split <= len(raw); split++ {
			s := NewStream(tc.format, config.Defaults().Limits, lookup)
			a, _, err := s.Push(raw[:split])
			if err != nil {
				t.Fatal(tc.format, split, err)
			}
			b, _, err := s.Push(raw[split:])
			if err != nil {
				t.Fatal(tc.format, split, err)
			}
			if got := joined(tc, decodeEvents(t, append(a, b...), "sse")); got != "中文\"\n\\secret" {
				t.Fatal(tc.format, split, got)
			}
		}
	}
	tc := textCases()[1]
	raw := []byte("[")
	raw = append(raw, encodeEvent(tc.delta(marker[:20], 0), "json")...)
	raw = append(raw, ',')
	raw = append(raw, encodeEvent(tc.delta(marker[20:], 0), "json")...)
	raw = append(raw, ',')
	raw = append(raw, encodeEvent(tc.stop(marker, 0), "json")...)
	raw = append(raw, ']')
	s := NewStream("gemini", config.Defaults().Limits, lookup)
	var out []byte
	for _, b := range raw {
		got, _, err := s.Push([]byte{b})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, got...)
	}
	if got := joined(tc, decodeEvents(t, out, "array")); got != "中文\"\n\\secret" {
		t.Fatal(got)
	}
}
func TestStreamLimitsAndMalformedEvents(t *testing.T) {
	for _, raw := range []string{`{"choices":[],"choices":[]}`, `{"choices":[{"index":"0","delta":{"content":"x"}}]}`, `{"error":{"message":"PRIVATE"}}`} {
		s := NewStream("openai", config.Defaults().Limits, lookup)
		if _, _, err := s.Push([]byte(raw)); err == nil {
			t.Fatal("accepted malformed event")
		}
	}
	l := config.Defaults().Limits
	l.EventBytes = 64
	s := NewStream("openai", l, lookup)
	if _, _, err := s.Push([]byte("data: " + strings.Repeat("x", 65))); err == nil {
		t.Fatal("event limit ignored")
	}
	l = config.Defaults().Limits
	l.RequestStateBytes = 200
	s = NewStream("openai", l, lookup)
	if _, _, err := s.Push(encodeEvent(textCases()[0].delta(marker[:10], 0), "json")); err == nil {
		t.Fatal("state limit ignored")
	}
	l = config.Defaults().Limits
	l.EventBytes = 256
	s = NewStream("openai", l, func(string) (string, bool) { return strings.Repeat("s", 1024), true })
	if _, _, err := s.Push(encodeEvent(textCases()[0].delta(marker, 0), "json")); err == nil {
		t.Fatal("restoration expansion limit ignored")
	}
}

func TestCPARecordStreamsAndOpaqueInteractionControl(t *testing.T) {
	for _, tc := range []streamCase{textCases()[3], textCases()[4]} {
		s := NewStream(tc.format, config.Defaults().Limits, lookup)
		var output []byte
		for _, m := range []map[string]any{tc.delta(marker[:20], 0), tc.delta(marker[20:], 0), tc.stop(marker, 0)} {
			raw := bytes.TrimRight(encodeEvent(m, "sse"), "\n")
			// Codex yields the event and data lines as two separate records.
			for _, record := range bytes.SplitN(raw, []byte("\n"), 2) {
				b, _, err := s.Push(record)
				if err != nil {
					t.Fatal(tc.format, err)
				}
				output = append(output, b...)
			}
		}
		if got := joined(tc, decodeEvents(t, output, "sse")); got != "中文\"\n\\secret" {
			t.Fatal(tc.format, got)
		}
	}
	s := NewStream("interactions", config.Defaults().Limits, lookup)
	for _, raw := range []string{
		`event: interaction.status_update` + "\ndata: " + `{"event_type":"interaction.status_update","status":"in_progress"}`,
		`event: step.delta` + "\ndata: " + `{"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"` + marker + `"}}`,
	} {
		b, _, err := s.Push([]byte(raw))
		if err != nil || len(b) == 0 {
			t.Fatal(err)
		}
		if strings.Contains(raw, marker) && !bytes.Contains(b, []byte(marker)) {
			t.Fatal("signature rewritten")
		}
	}
	if got := joined(textCases()[4], pushEvent(t, s, textCases()[4], textCases()[4].delta(marker, 1))); got != "中文\"\n\\secret" {
		t.Fatal("control event stopped restoration")
	}
}
