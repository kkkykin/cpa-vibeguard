package restore

import (
	"fmt"
	"strings"
	"testing"
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
