package rules

import (
	"context"
	"strings"
	"testing"
)

func TestScanMatchLimitIsAnError(t *testing.T) {
	e := testEngine(t, custom("        - {id: repeated, regex: x}\n"))
	b, err := e.Bind(Input{})
	if err != nil {
		t.Fatal(err)
	}
	if spans, err := b.Scan(context.Background(), strings.Repeat("x", MaxScanMatches+1)); err == nil || len(spans) > 0 {
		t.Fatal("partial over-limit scan returned as success")
	}
}
