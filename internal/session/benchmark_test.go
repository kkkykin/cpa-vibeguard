package session

import (
	"fmt"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

func BenchmarkHistoryReferences(b *testing.B) {
	for _, sessions := range []int{1, 128, 1024} {
		for _, operation := range []string{"resolve", "record"} {
			b.Run(fmt.Sprintf("%d-sessions/%s", sessions, operation), func(b *testing.B) {
				store := token.NewStore()
				b.Cleanup(store.Close)
				r := New(store)
				cfg := config.Defaults()
				scfg := config.DefaultSession()
				cfg.Session = &scfg
				var first *token.Lease
				for i := 0; i < sessions; i++ {
					lease, err := store.Acquire(fmt.Sprintf("bench-%d", i), true, limits(&cfg))
					if err != nil {
						b.Fatal(err)
					}
					if i == 0 {
						first = lease
					} else {
						lease.Release()
					}
				}
				refs := make([]protocol.Reference, MaxReferences)
				for i := range refs {
					refs[i] = protocol.Reference{Kind: "response", ID: fmt.Sprintf("resp-%d", i)}
				}
				selection := Selection{Lease: first, Namespace: "bench", Caller: "caller", MaxIDBytes: 1024}
				if err := r.Record(selection, refs); err != nil {
					b.Fatal(err)
				}
				in := Input{RequestID: "next", SourceFormat: "openai-response", Metadata: map[string]any{"caller_scope": "caller"}, Previous: refs[:1]}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if operation == "resolve" {
						selected, err := r.Resolve(&cfg, in)
						if err != nil {
							b.Fatal(err)
						}
						selected.Lease.Release()
					} else if err := r.Record(selection, refs[:1]); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
