package restore

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

const marker = "[[redacted:v1:0123456789abcdef0123456789abcdef]]"

func TestRecordFramingDoesNotReparseGrowingString(t *testing.T) {
	raw := []byte(`data: {"text":"` + strings.Repeat("}", 16<<10) + `"}`)
	allocations := testing.AllocsPerRun(1, func() {
		f := Framer{Limit: 32 << 10, Records: true}
		count := 0
		for i := range raw {
			frames, err := f.Push(raw[i : i+1])
			if err != nil {
				t.Fatal(err)
			}
			count += len(frames)
		}
		if count != 1 || f.Buffered() != 0 {
			t.Fatal("fragmented record was lost or retained")
		}
	})
	// Buffer growth and the final frame take a bounded number of allocations.
	// Re-parsing every closing brace would allocate tens of thousands of times.
	if allocations > 200 {
		t.Fatalf("repeated parsing of incomplete JSON: %.0f allocations", allocations)
	}
}

func TestRecordFramingIgnoresMetadataAndPreservesPartialLines(t *testing.T) {
	raw := []byte("event: metadata contains \"{]}\"\r\ndata: {\"items\":[\ndata: \"}\\\"{\",\ndata: {\"v\":\"}\"}]}")
	f := Framer{Limit: 1024, Records: true}
	var frames []Frame
	for i := range raw {
		got, err := f.Push(raw[i : i+1])
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, got...)
	}
	if len(frames) != 1 || !json.Valid(frames[0].Data) || len(frames[0].Lines) != 1 {
		t.Fatalf("metadata affected JSON balance: %+v", frames)
	}
	f = Framer{Limit: 1024, Records: true}
	if frames, err := f.Push([]byte("data: {}\nid: hello")); err != nil || len(frames) != 0 {
		t.Fatal("partial metadata line delivered prematurely")
	}
	frames, err := f.Push([]byte(" world\n\n"))
	if err != nil || len(frames) != 1 || !strings.Contains(string(frames[0].Render(frames[0].Data)), "id: hello world") {
		t.Fatalf("metadata fragment lost: %v %+v", err, frames)
	}
}

func lookup(s string) (string, bool) {
	if s == marker {
		return "中文\"\n\\secret", true
	}
	return "", false
}

func TestTextEveryMarkerSplitAndBound(t *testing.T) {
	for split := 0; split <= len(marker); split++ {
		stream := NewText(lookup)
		prefix := stream.Push("before "+marker[:split], false)
		if stream.Buffered() > 47 {
			t.Fatal("marker candidate exceeded 47 bytes")
		}
		got := prefix + stream.Push(marker[split:]+" after", false) + stream.Push("", true)
		if got != "before 中文\"\n\\secret after" {
			t.Fatalf("split %d: %q", split, got)
		}
	}
	stream := NewText(lookup)
	var out strings.Builder
	for _, b := range []byte("[[" + marker + marker + "[[unknown:v1:thing]]") {
		out.WriteString(stream.Push(string([]byte{b}), false))
		if stream.Buffered() > 47 {
			t.Fatal("candidate too large")
		}
	}
	out.WriteString(stream.Push("", true))
	if out.String() != "[[中文\"\n\\secret中文\"\n\\secret[[unknown:v1:thing]]" {
		t.Fatal(out.String())
	}
}

func TestTextNormalAndAbnormalTail(t *testing.T) {
	for split := 1; split < len(marker); split++ {
		s := NewText(lookup)
		got := s.Push("delivered "+marker[:split], false)
		if got != "delivered " {
			t.Fatalf("partial marker escaped at %d: %q", split, got)
		}
		s.Abort()
		if s.Buffered() != 0 {
			t.Fatal("abort retained tail")
		}
		s = NewText(lookup)
		got = s.Push("delivered "+marker[:split], false) + s.Push("", true)
		if got != "delivered "+marker[:split] {
			t.Fatal("normal end did not preserve incomplete marker")
		}
	}
	unknown := token.Prefix + strings.Repeat("f", 32) + "]]"
	s := NewText(lookup)
	if got := s.Push(unknown, true); got != unknown {
		t.Fatal("unknown marker guessed")
	}
	nested := NewText(func(s string) (string, bool) { return marker, true })
	if nested.Push(marker, true) != marker {
		t.Fatal("recursive replacement")
	}
}

func TestToolJSONEverySplit(t *testing.T) {
	for _, argument := range []string{
		`{"q":"` + marker + `","nested":["` + marker + `",42,true,null],"n":9007199254740993}`,
		`{"` + marker + `":"` + marker + `","unicode":"\ud83d\ude00"}`,
		`["` + strings.ReplaceAll(strings.ReplaceAll(marker, "[", `\u005b`), "]", `\u005d`) + `","中文"]`,
	} {
		for split := 0; split <= len(argument); split++ {
			codec := NewToolJSON(lookup, 4096)
			a, err := codec.Push(argument[:split], false)
			if err != nil {
				t.Fatalf("split %d: %v", split, err)
			}
			b, err := codec.Push(argument[split:], true)
			if err != nil {
				t.Fatalf("split %d: %v", split, err)
			}
			if !json.Valid([]byte(a + b)) {
				t.Fatalf("split %d produced invalid JSON: %s", split, a+b)
			}
			if !strings.Contains(a+b, `\"\n\\secret`) {
				t.Fatalf("secret was not escaped as JSON: %s", a+b)
			}
			if strings.Contains(argument, "9007199254740993") && !strings.Contains(a+b, "9007199254740993") {
				t.Fatal("number rounded")
			}
			if strings.HasPrefix(argument, `{"`+marker) && !strings.HasPrefix(a+b, `{"`+marker) {
				t.Fatal("tool key was rewritten")
			}
			if codec.Buffered() != 0 {
				t.Fatalf("completed codec retained %d bytes", codec.Buffered())
			}
		}
	}
}

func TestToolJSONReleasesCompleteMarkersImmediately(t *testing.T) {
	c := NewToolJSON(lookup, 1024)
	out, err := c.Push(`{"q":"`+marker, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `secret`) {
		t.Fatal("complete marker buffered until end of tool arguments")
	}
	tail, err := c.Push(`"}`, true)
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]string
	if err := json.Unmarshal([]byte(out+tail), &actual); err != nil {
		t.Fatal(err)
	}
	if actual["q"] != "中文\"\n\\secret" {
		t.Fatal(actual)
	}
}

func TestToolJSONRejectsMalformedAndBoundedArguments(t *testing.T) {
	for _, argument := range []string{`{"a":1,"a":2}`, `{"a":"\q"}`, `{"a":"\ud800"}`, `{"a":[1,]}`, `{"a":}`, `{"a":"unfinished`, `{"a":true} {}`, `"scalar"`} {
		c := NewToolJSON(lookup, 1024)
		if _, err := c.Push(argument, true); err == nil {
			t.Fatalf("accepted %s", argument)
		}
		c.Abort()
		if c.Buffered() != 0 {
			t.Fatal("abort retained tool state")
		}
	}
	c := NewToolJSON(lookup, 10)
	if _, err := c.Push(`{"q":"too long"}`, true); err == nil {
		t.Fatal("tool limit ignored")
	}
}

func TestSSEFramingAllSplits(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		stream := strings.ReplaceAll(": heartbeat\n\nevent: response.output_text.delta\nid: 12\ndata: {\ndata: \"delta\":\"text\"}\n\ndata: [DONE]\n\n", "\n", newline)
		for split := 0; split <= len(stream); split++ {
			f := &Framer{Limit: 1024}
			a, err := f.Push([]byte(stream[:split]))
			if err != nil {
				t.Fatalf("split %d: %v", split, err)
			}
			b, err := f.Push([]byte(stream[split:]))
			if err != nil {
				t.Fatal(err)
			}
			frames := append(a, b...)
			if len(frames) != 3 || frames[0].HasData || !json.Valid(frames[1].Data) || string(frames[2].Data) != "[DONE]" || f.Buffered() != 0 {
				t.Fatalf("split %d: %+v", split, frames)
			}
			rendered := frames[1].Render([]byte(`{"delta":"rewritten"}`))
			if !bytes.Contains(rendered, []byte("id: 12")) || !bytes.Contains(rendered, []byte("event: response.output_text.delta")) {
				t.Fatal("SSE metadata lost")
			}
		}
	}
}

func TestJSONAndArrayFraming(t *testing.T) {
	for _, stream := range []string{`{"text":"}\\\"["}`, `[{"text":"}"},{"next":[1,2]}]`} {
		f := &Framer{Limit: 1024}
		var frames []Frame
		for _, b := range []byte(stream) {
			got, err := f.Push([]byte{b})
			if err != nil {
				t.Fatal(err)
			}
			frames = append(frames, got...)
		}
		if stream[0] == '[' {
			if len(frames) != 3 || !frames[2].Close {
				t.Fatal(frames)
			}
		} else if len(frames) != 1 {
			t.Fatal(frames)
		}
		for _, frame := range frames {
			if !frame.Close && !json.Valid(frame.Data) {
				t.Fatal(string(frame.Data))
			}
		}
	}
	f := &Framer{Limit: 12}
	if _, err := f.Push([]byte("data: " + strings.Repeat("x", 13))); err == nil {
		t.Fatal("SSE event limit ignored")
	}
	f.Abort()
	if f.Buffered() != 0 {
		t.Fatal("framing buffer retained")
	}
	f = &Framer{Limit: 1024}
	if _, err := f.Push([]byte(`[{"a":1},]`)); err == nil {
		t.Fatal("trailing stream array comma accepted")
	}
}

func TestHostSSERecordsAndRawTransportAreDistinct(t *testing.T) {
	for _, raw := range []string{`data: {"delta":"value"}`, "event: step.stop\ndata: {\"event_type\":\"step.stop\"}", `data: [DONE]`} {
		f := &Framer{Limit: 1024, Records: true}
		frames, err := f.Push([]byte(raw))
		if err != nil || len(frames) != 1 || f.Buffered() != 0 {
			t.Fatalf("record: %v %+v", err, frames)
		}
		f = &Framer{Limit: 1024}
		frames, err = f.Push([]byte(raw))
		if err != nil || len(frames) != 0 {
			t.Fatal("raw SSE was prematurely dispatched")
		}
		frames, err = f.Push([]byte("\nid: trailing-id\n\n"))
		if err != nil || len(frames) != 1 || !bytes.Contains(frames[0].Render(frames[0].Data), []byte("trailing-id")) {
			t.Fatal("raw transport lost metadata after data")
		}
	}
}
