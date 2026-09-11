// Package restore restores authorized markers in bounded protocol streams.
package restore

import (
	"strings"

	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

type Lookup func(string) (string, bool)
type Text struct {
	pending string
	lookup  Lookup
}

func NewText(lookup Lookup) *Text { return &Text{lookup: lookup} }
func (t *Text) Push(fragment string, final bool) string {
	input := t.pending + fragment
	t.pending = ""
	var out strings.Builder
	for offset := 0; offset < len(input); {
		rel := strings.IndexByte(input[offset:], '[')
		if rel < 0 {
			out.WriteString(input[offset:])
			break
		}
		start := offset + rel
		out.WriteString(input[offset:start])
		end := start + token.Length
		if end > len(input) {
			end = len(input)
		}
		candidate := input[start:end]
		if !token.Possible(candidate) {
			out.WriteByte('[')
			offset = start + 1
			continue
		}
		if len(candidate) < token.Length {
			if final {
				out.WriteString(candidate)
			} else {
				t.pending = strings.Clone(candidate)
			}
			break
		}
		if original, ok := t.lookup(candidate); ok {
			out.WriteString(original)
		} else {
			out.WriteString(candidate)
		}
		offset = end
	}
	return out.String()
}
func (t *Text) Buffered() int { return len(t.pending) }
func (t *Text) Abort()        { t.pending = "" }
