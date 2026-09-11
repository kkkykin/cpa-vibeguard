package rules

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
)

func benchmarkRules(b *testing.B, raw string) *Engine {
	b.Helper()
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		b.Fatal(err)
	}
	e, err := New(cfg, nil)
	if err != nil {
		b.Fatal(err)
	}
	return e
}

func BenchmarkRuleScan(b *testing.B) {
	const custom = `rule_sources: [{inline: {extend: {useDefault: false}, rules: [{id: fixture, regex: 'PRIVATE-[A-Z0-9]{16}', keywords: [PRIVATE-]}]}}]`
	clean := strings.Repeat("func example() { return public_fixture }\n", 256)
	for _, tc := range []struct {
		name, config, text string
		wantMatch          bool
	}{
		{"custom/clean", custom, clean, false},
		{"custom/match", custom, clean + "PRIVATE-0123456789ABCDEF\n", true},
		{"builtin/clean", "", clean, false},
		// This is a synthetic, nonworking fixture matching the PAT syntax.
		{"builtin/match", "", clean + "github = ghp_0123456789abcdef0123456789ABCDEFGHijkl\n", true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			e := benchmarkRules(b, tc.config)
			bound, err := e.Bind(Input{})
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(tc.text)))
			b.ReportAllocs()
			for b.Loop() {
				spans, err := bound.Scan(context.Background(), tc.text)
				if err != nil || (len(spans) > 0) != tc.wantMatch {
					b.Fatalf("unexpected scan result: %d matches, %v", len(spans), err)
				}
			}
			b.ReportMetric(float64(len(e.RuleIDs())), "rules")
		})
	}
}

func BenchmarkVariableBinding(b *testing.B) {
	const cfg = `variables: {tenant: {header: X-Tenant}}
rule_sources: [{inline: {extend: {useDefault: false}, rules: [{id: tenant, regex: '{var.tenant}', keywords: ['{var.tenant}']}]}}]`
	for _, hit := range []bool{true, false} {
		name := "miss"
		if hit {
			name = "hit"
		}
		b.Run(name, func(b *testing.B) {
			e := benchmarkRules(b, cfg)
			input := Input{Headers: http.Header{"X-Tenant": {"tenant.fixture-0"}}}
			if _, err := e.Bind(input); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				if !hit {
					input.Headers["X-Tenant"][0] = fmt.Sprintf("tenant.fixture-%d", i)
				}
				if bound, err := e.Bind(input); err != nil || bound == nil {
					b.Fatal(err)
				}
			}
			stats := e.Stats()
			if stats.Entries > CacheEntries || stats.Bytes > CacheBytes {
				b.Fatal("binding cache exceeded its limits")
			}
			b.ReportMetric(float64(stats.Entries), "cached-entries")
		})
	}
}
