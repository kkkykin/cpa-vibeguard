package token

import (
	"errors"
	"strings"
	"testing"
)

func TestRewriteBoundedExactSinglePass(t *testing.T) {
	marker := Prefix + strings.Repeat("a", 32) + "]]"
	other := Prefix + strings.Repeat("b", 32) + "]]"
	original := "秘密\n\"\\" + other
	lookup := func(m string) (string, bool) {
		if m == marker {
			return original, true
		}
		return "recursive replacement", m == other
	}
	for _, tc := range []struct{ input, want string }{
		{"", ""},
		{"plain text", "plain text"},
		{marker, original},
		{"before " + marker + " after", "before " + original + " after"},
		{marker + marker, original + original},
		{marker[:Length-1], marker[:Length-1]},
		{Prefix + strings.Repeat("c", 32) + "]]", Prefix + strings.Repeat("c", 32) + "]]"},
	} {
		out, err := RewriteBounded(tc.input, lookup, len(tc.want))
		if err != nil || out != tc.want || Rewrite(tc.input, lookup) != tc.want {
			t.Fatalf("bounded rewrite: %v %q, want %q", err, out, tc.want)
		}
		if len(tc.want) > 0 {
			if out, err := RewriteBounded(tc.input, lookup, len(tc.want)-1); !errors.Is(err, ErrRestoreLimit) || out != "" {
				t.Fatal("overflow returned partial text")
			}
		}
	}
	// Shrinking replacements can fit even when the input itself exceeds the
	// output limit. Budgeting follows delivered bytes rather than input size.
	if out, err := RewriteBounded(marker+marker, func(string) (string, bool) { return "x", true }, 2); err != nil || out != "xx" {
		t.Fatalf("shrinking restoration: %q %v", out, err)
	}
}

func TestRewriteStopsBeforeLargeExpansion(t *testing.T) {
	marker := Prefix + strings.Repeat("a", 32) + "]]"
	original := strings.Repeat("secret", 1<<18)
	lookups := 0
	// An unchecked expansion would exceed 15 GiB. Only the first lookup is
	// needed to reject this response; no replacement bytes should be copied.
	out, err := RewriteBounded(strings.Repeat(marker, 10000), func(string) (string, bool) {
		lookups++
		return original, true
	}, 1024)
	if !errors.Is(err, ErrRestoreLimit) || out != "" || lookups != 1 {
		t.Fatalf("unbounded restoration: calls=%d bytes=%d err=%v", lookups, len(out), err)
	}
}
