package session

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

func retentionSession(t *testing.T, r *Resolver, c *config.Config, caller, id string) Selection {
	t.Helper()
	in := input(caller + "/" + id)
	in.Metadata["caller_scope"] = caller
	in.Headers = http.Header{"X-Session": {id}}
	return resolve(t, r, c, in)
}

func retentionRefs(prefix string, count int) []protocol.Reference {
	refs := make([]protocol.Reference, count)
	for i := range refs {
		refs[i] = protocol.Reference{Kind: "response", ID: fmt.Sprintf("%s-%04d", prefix, i)}
	}
	return refs
}

func TestReferenceLRUAndSharedCallerQuota(t *testing.T) {
	store := token.NewStore()
	defer store.Close()
	r := New(store)
	c := cfg(t, sessionConfig)
	first := retentionSession(t, r, c, "caller-A", "first")
	marker := allocate(t, first, "secret")
	refs := retentionRefs("a", MaxCallerReferences)
	if err := r.Record(first, refs); err != nil {
		t.Fatal(err)
	}
	other := retentionSession(t, r, c, "caller-B", "same")
	otherRef := protocol.Reference{Kind: "response", ID: "other"}
	if err := r.Record(other, []protocol.Reference{otherRef}); err != nil {
		t.Fatal(err)
	}
	// Reading the oldest reference keeps it ahead of untouched references.
	in := input("read-oldest")
	in.Previous = refs[:1]
	resolve(t, r, c, in).Lease.Release()
	c.Session.Profiles[0].Namespace = "alternate"
	second := retentionSession(t, r, c, "caller-A", "second")
	newRef := protocol.Reference{Kind: "response", ID: "new"}
	if err := r.Record(second, []protocol.Reference{newRef}); err != nil {
		t.Fatal(err)
	}
	c.Session.Profiles[0].Namespace = "team"
	in.Previous = refs[1:2]
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("least recently used reference survived caller quota")
	}
	in.Previous = refs[:1]
	if selected := resolve(t, r, c, in); selected.Lease.Key() != first.Lease.Key() {
		t.Fatal("retained history moved to another session")
	}
	// Changing session IDs or namespaces cannot multiply the caller quota.
	for i := 0; i < 3; i++ {
		if err := r.Record(second, retentionRefs(fmt.Sprintf("rotate-%d", i), MaxCallerReferences)); err != nil {
			t.Fatal(err)
		}
	}
	if stats := r.Stats(); stats.References != MaxCallerReferences+1 || stats.Bytes > 2*MaxCallerReferenceBytes {
		t.Fatalf("caller quota exceeded: %+v", stats)
	}
	in.Metadata["caller_scope"] = "caller-B"
	in.Previous = []protocol.Reference{otherRef}
	if selected := resolve(t, r, c, in); selected.Lease.Key() != other.Lease.Key() {
		t.Fatal("busy caller evicted another caller's history")
	}
	if first.Lease.Restore(marker) != "secret" {
		t.Fatal("reference eviction removed an active secret mapping")
	}
}

func TestReferenceRegistrationIsAtomicBeforeEviction(t *testing.T) {
	store := token.NewStore()
	defer store.Close()
	r := New(store)
	c := cfg(t, sessionConfig)
	first := retentionSession(t, r, c, "caller-A", "first")
	second := retentionSession(t, r, c, "caller-A", "second")
	refs := retentionRefs("old", MaxCallerReferences)
	if err := r.Record(first, refs); err != nil {
		t.Fatal(err)
	}
	before := r.Stats()
	fresh := protocol.Reference{Kind: "response", ID: "fresh"}
	oversized := retentionRefs("fresh", MaxCallerReferences+1)
	for _, tc := range []struct {
		name      string
		selection Selection
		refs      []protocol.Reference
		capacity  bool
	}{
		{"invalid", first, []protocol.Reference{fresh, {Kind: "response", ID: " invalid "}}, false},
		{"conflict", second, []protocol.Reference{fresh, refs[0]}, false},
		{"oversized-invalid", first, append(append([]protocol.Reference{}, oversized...), protocol.Reference{Kind: "response", ID: " invalid "}), false},
		{"oversized-conflict", second, append(append([]protocol.Reference{}, oversized...), refs[0]), false},
		{"old-and-new-exceed-batch-quota", first, append(append([]protocol.Reference{}, refs...), fresh), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := r.Record(tc.selection, tc.refs)
			if err == nil || errors.Is(err, ErrReferenceCapacity) != tc.capacity {
				t.Fatalf("wrong registration failure: %v", err)
			}
			if after := r.Stats(); after != before {
				t.Fatalf("failed batch mutated retention: before=%+v after=%+v", before, after)
			}
			in := input("unpublished")
			in.Previous = []protocol.Reference{fresh}
			if _, err := r.Resolve(c, in); err == nil {
				t.Fatal("failed batch partially published a reference")
			}
		})
	}
	// A failed history batch must not refresh even its valid prefix.
	in := input("failed-read")
	in.Previous = []protocol.Reference{refs[0], fresh}
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("partially known history accepted")
	}
	if err := r.Record(first, []protocol.Reference{fresh}); err != nil {
		t.Fatal(err)
	}
	in.Previous = refs[:1]
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("failed batch refreshed its valid prefix or changed eviction order")
	}
	// Re-observing an existing ID refreshes it, including duplicate batches.
	duplicates := make([]protocol.Reference, MaxCallerReferences+10)
	for i := range duplicates {
		duplicates[i] = refs[1]
	}
	if err := r.Record(first, duplicates); err != nil {
		t.Fatalf("duplicate IDs consumed extra capacity: %v", err)
	}
	if err := r.Record(first, []protocol.Reference{{Kind: "response", ID: "next"}}); err != nil {
		t.Fatal(err)
	}
	in.Previous = refs[1:2]
	resolve(t, r, c, in).Lease.Release()
	in.Previous = refs[2:3]
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("recording an existing ID did not refresh LRU retention")
	}
}

func TestReferenceByteBudgets(t *testing.T) {
	store := token.NewStore()
	defer store.Close()
	r := New(store)
	c := cfg(t, sessionConfig)
	c.Session.MaxIDBytes = MaxCallerReferenceBytes
	selected := retentionSession(t, r, c, "caller-A", "first")
	tooLarge := protocol.Reference{Kind: "response", ID: strings.Repeat("x", MaxCallerReferenceBytes)}
	if err := r.Record(selected, []protocol.Reference{tooLarge}); !errors.Is(err, ErrReferenceCapacity) || r.Stats() != (Stats{}) {
		t.Fatalf("oversized ID was published: %v %+v", err, r.Stats())
	}
	var owners []Selection
	var refs []protocol.Reference
	for i := 0; i < 9; i++ {
		owner := retentionSession(t, r, c, fmt.Sprintf("owner-%d", i), "same")
		ref := protocol.Reference{Kind: "response", ID: strings.Repeat("x", MaxCallerReferenceBytes-2048) + fmt.Sprint(i)}
		if err := r.Record(owner, []protocol.Reference{ref}); err != nil {
			t.Fatal(err)
		}
		owners = append(owners, owner)
		refs = append(refs, ref)
	}
	if stats := r.Stats(); stats.References != 8 || stats.Bytes > MaxReferenceBytes {
		t.Fatalf("global byte limit ignored: %+v", stats)
	}
	in := input("evicted")
	in.Metadata["caller_scope"] = "owner-0"
	in.Previous = refs[:1]
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("global byte eviction retained the oldest reference")
	}
	// Another large ID for an existing caller must evict that caller's old
	// ID first, leaving other callers' references available.
	latest := protocol.Reference{Kind: "response", ID: refs[8].ID + "-new"}
	if err := r.Record(owners[8], []protocol.Reference{latest}); err != nil {
		t.Fatal(err)
	}
	in.Metadata["caller_scope"] = "owner-8"
	in.Previous = refs[8:9]
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("per-caller byte quota ignored")
	}
	in.Previous = []protocol.Reference{latest}
	resolve(t, r, c, in).Lease.Release()
	in.Metadata["caller_scope"] = "owner-1"
	in.Previous = refs[1:2]
	resolve(t, r, c, in).Lease.Release()
	if stats := r.Stats(); stats.References != 8 || stats.Bytes > MaxReferenceBytes {
		t.Fatal(stats)
	}
}

func TestConcurrentReferenceRetention(t *testing.T) {
	store := token.NewStore()
	defer store.Close()
	r := New(store)
	c := cfg(t, sessionConfig)
	var owners []Selection
	for i := 0; i < MaxReferences/MaxCallerReferences; i++ {
		owner := retentionSession(t, r, c, fmt.Sprintf("owner-%d", i), "same")
		if err := r.Record(owner, retentionRefs(fmt.Sprint(i), MaxCallerReferences)); err != nil {
			t.Fatal(err)
		}
		owners = append(owners, owner)
	}
	var wg sync.WaitGroup
	for i, owner := range owners {
		wg.Go(func() {
			for j := 0; j < 32; j++ {
				ref := protocol.Reference{Kind: "response", ID: fmt.Sprintf("next-%d-%d", i, j)}
				if err := r.Record(owner, []protocol.Reference{ref}); err != nil {
					t.Error(err)
					return
				}
				in := input(fmt.Sprintf("request-%d-%d", i, j))
				in.Metadata["caller_scope"] = owner.Caller
				in.Previous = []protocol.Reference{ref}
				selected, err := r.Resolve(c, in)
				if err != nil {
					t.Error(err)
					return
				}
				if selected.Lease.Key() != owner.Lease.Key() || selected.Lease.Generation() != owner.Lease.Generation() {
					t.Error("concurrent registration crossed session generations")
				}
				selected.Lease.Release()
			}
		})
	}
	wg.Go(func() {
		for i := 0; i < 64; i++ {
			r.Stats()
		}
	})
	wg.Wait()
	if stats := r.Stats(); stats.References != MaxReferences || stats.Bytes > MaxReferenceBytes {
		t.Fatal(stats)
	}
}
