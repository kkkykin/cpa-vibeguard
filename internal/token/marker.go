// Package token owns random markers and the scoped, in-memory secret store.
package token

import (
	"errors"
	"strings"
)

const Prefix = "[[redacted:v1:"
const Length = 48

var ErrRestoreLimit = errors.New("restored_text_size_limit")

func Possible(s string) bool {
	if len(s) > Length {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch {
		case i < len(Prefix):
			if s[i] != Prefix[i] {
				return false
			}
		case i < Length-2:
			if !((s[i] >= '0' && s[i] <= '9') || (s[i] >= 'a' && s[i] <= 'f')) {
				return false
			}
		default:
			if s[i] != ']' {
				return false
			}
		}
	}
	return true
}
func Valid(s string) bool { return len(s) == Length && Possible(s) }

// Rewrite is a single pass over the supplied text. Replacement plaintext is
// never scanned again, even if it happens to contain another valid marker.
func Rewrite(text string, lookup func(string) (string, bool)) string {
	out, _ := RewriteBounded(text, lookup, int(^uint(0)>>1))
	return out
}

// RewriteBounded checks each append before allocating replacement bytes. The
// caller may share the remaining budget across all selected response fields.
// It returns no partial result on overflow and never scans restored plaintext.
func RewriteBounded(text string, lookup func(string) (string, bool), limit int) (string, error) {
	if limit < 0 {
		return "", ErrRestoreLimit
	}
	var out strings.Builder
	last, offset := 0, 0
	for offset < len(text) {
		rel := strings.Index(text[offset:], Prefix)
		if rel < 0 {
			break
		}
		start := offset + rel
		end := start + Length
		if end > len(text) {
			break
		}
		marker := text[start:end]
		if Valid(marker) {
			if original, ok := lookup(marker); ok {
				literal := text[last:start]
				if len(literal) > limit-out.Len() || len(original) > limit-out.Len()-len(literal) {
					return "", ErrRestoreLimit
				}
				out.WriteString(literal)
				out.WriteString(original)
				last = end
			}
			offset = end
		} else {
			offset = start + 1
		}
	}
	if last == 0 {
		if len(text) > limit {
			return "", ErrRestoreLimit
		}
		return text, nil
	}
	if len(text)-last > limit-out.Len() {
		return "", ErrRestoreLimit
	}
	out.WriteString(text[last:])
	return out.String(), nil
}
