package restore

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
)

func BenchmarkFramerRecordByteFragments(b *testing.B) {
	for _, size := range []int{4 << 10, 64 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			raw := []byte(`data: {"text":"` + strings.Repeat("}", size) + `"}`)
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for b.Loop() {
				f := Framer{Limit: size + 128, Records: true}
				count := 0
				for i := range raw {
					frames, err := f.Push(raw[i : i+1])
					if err != nil {
						b.Fatal(err)
					}
					count += len(frames)
				}
				if count != 1 {
					b.Fatal("record missing")
				}
			}
		})
	}
}

func BenchmarkTextMarkerLatency(b *testing.B) {
	text := NewText(lookup)
	b.SetBytes(int64(len(marker)))
	b.ReportAllocs()
	for b.Loop() {
		if text.Push(marker[:47], false) != "" || text.Buffered() != 47 {
			b.Fatal("partial marker was delivered")
		}
		if text.Push(marker[47:], false) != "中文\"\n\\secret" || text.Buffered() != 0 {
			b.Fatal("complete marker was not restored immediately")
		}
	}
	b.ReportMetric(47, "peak-tail-B")
}

func BenchmarkLongStream(b *testing.B) {
	for _, chunks := range []int{256, 4096} {
		b.Run(fmt.Sprint(chunks)+"-chunks", func(b *testing.B) {
			tc := textCases()[0]
			parts := [][]byte{
				encodeEvent(tc.delta(strings.Repeat("a", 1024)+marker[:20], 0), "json"),
				encodeEvent(tc.delta(marker[20:]+strings.Repeat("b", 1024), 0), "json"),
			}
			stop := encodeEvent(tc.stop("", 0), "json")
			b.SetBytes(int64(chunks/2*(len(parts[0])+len(parts[1])) + len(stop)))
			b.ReportAllocs()
			peak := 0
			for b.Loop() {
				s := NewStream("openai", config.Defaults().Limits, lookup)
				for i := 0; i < chunks; i++ {
					if _, _, err := s.Push(parts[i%2]); err != nil {
						b.Fatal(err)
					}
					peak = max(peak, s.Buffered())
				}
				if _, _, err := s.Push(stop); err != nil {
					b.Fatal(err)
				}
				s.Abort()
				if s.Buffered() != 0 {
					b.Fatal("stream retained state after abort")
				}
			}
			b.ReportMetric(float64(peak), "peak-state-B")
		})
	}
}

func BenchmarkToolJSONFragments(b *testing.B) {
	argument := `{"query":"` + strings.Repeat("plain ", 2048) + marker + `","nested":["中文",9007199254740993]}`
	b.SetBytes(int64(len(argument)))
	b.ReportAllocs()
	peak := 0
	for b.Loop() {
		tool := NewToolJSON(lookup, 64<<10)
		for i := 0; i < len(argument); i += 23 {
			end := min(len(argument), i+23)
			if _, err := tool.Push(argument[i:end], end == len(argument)); err != nil {
				b.Fatal(err)
			}
			peak = max(peak, tool.Buffered())
		}
		if tool.Buffered() != 0 {
			b.Fatal("completed tool retained its validation buffer")
		}
	}
	b.ReportMetric(float64(peak), "peak-tool-B")
}
