package session

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

const sessionConfig = `session:
  profiles:
    - namespace: team
      sources:
        - header: X-Session
        - json: /metadata/a~1b/0
        - metadata: /execution_session_id
`

func cfg(t *testing.T, raw string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func input(id string) Input {
	return Input{RequestID: id, SourceFormat: "openai-response", RequestedModel: "model", JSON: map[string]any{}, Metadata: map[string]any{"caller_scope": "caller-A"}}
}
func resolve(t *testing.T, r *Resolver, c *config.Config, in Input) Selection {
	t.Helper()
	s, err := r.Resolve(c, in)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Lease.Release)
	return s
}
func allocate(t *testing.T, s Selection, secret string) string {
	t.Helper()
	tx, err := s.Lease.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	m, err := tx.Marker(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOrderedFallbackAndCrossSourceReuse(t *testing.T) {
	store := token.NewStore()
	defer store.Close()
	r := New(store)
	c := cfg(t, sessionConfig)
	in := input("one")
	in.Headers = http.Header{"x-session": {"session-1"}}
	in.JSON = map[string]any{"metadata": false}
	s := resolve(t, r, c, in)
	m := allocate(t, s, "secret")
	s.Lease.Release()
	in = input("two")
	in.JSON = map[string]any{"metadata": map[string]any{"a/b": []any{"session-1"}}}
	in.Metadata["execution_session_id"] = "different"
	next := resolve(t, r, c, in)
	if allocate(t, next, "secret") != m || next.Lease.Key() != s.Lease.Key() {
		t.Fatal("source kind or position changed scope")
	}
	next.Lease.Release()
	in = input("three")
	in.Headers = http.Header{"X-Session": {""}}
	in.JSON = map[string]any{"metadata": nil}
	in.Metadata["execution_session_id"] = "session-1"
	// A null intermediate container is invalid, while a null selected value is
	// a fallback. Use an explicitly null selected pointer here.
	in.JSON = map[string]any{"metadata": map[string]any{"a/b": []any{nil}}}
	next = resolve(t, r, c, in)
	if allocate(t, next, "secret") != m {
		t.Fatal("metadata fallback lost scope")
	}
	in = input("new")
	in.Headers = http.Header{"X-Session": {"unmapped-first"}}
	in.Metadata["execution_session_id"] = "session-1"
	other := resolve(t, r, c, in)
	if allocate(t, other, "secret") == m {
		t.Fatal("unknown first source fell back to an existing mapping")
	}
}

func TestInvalidSourcesDoNotFallback(t *testing.T) {
	store := token.NewStore()
	defer store.Close()
	r := New(store)
	c := cfg(t, sessionConfig)
	for _, value := range []any{false, 12, []any{}, map[string]any{}, " trailing ", "bad\nvalue", strings.Repeat("x", 1025)} {
		in := input("bad")
		in.JSON = map[string]any{"metadata": map[string]any{"a/b": []any{value}}}
		in.Metadata["execution_session_id"] = "fallback"
		if _, err := r.Resolve(c, in); err == nil {
			t.Fatalf("invalid source fell back: %T", value)
		}
	}
	for _, values := range [][]string{{"a", "b"}, {}} {
		in := input("bad")
		in.Headers = http.Header{"X-Session": values}
		in.Metadata["execution_session_id"] = "fallback"
		if _, err := r.Resolve(c, in); err == nil {
			t.Fatal("ambiguous header fell back")
		}
	}
	in := input("identity")
	in.Headers = http.Header{"X-Session": {"valid"}, "X-User": {"forged"}}
	delete(in.Metadata, "caller_scope")
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("client identity header trusted")
	}
}

func TestProfileSelectionAndIsolation(t *testing.T) {
	store := token.NewStore()
	defer store.Close()
	r := New(store)
	c := cfg(t, `session:
  profiles:
    - namespace: specialized
      match: {source_format: gemini, requested_model_prefix: upstream/}
      sources: [{header: X-Special}]
    - namespace: default
      sources: [{header: X-Session}]
`)
	in := input("a")
	in.Headers = http.Header{"X-Session": {"same"}, "X-Special": {"same"}}
	a := resolve(t, r, c, in)
	marker := allocate(t, a, "secret")
	in.RequestID = "b"
	in.SourceFormat = "gemini"
	in.RequestedModel = "upstream/model"
	b := resolve(t, r, c, in)
	if allocate(t, b, "secret") == marker {
		t.Fatal("namespace isolation failed")
	}
	in.RequestID = "c"
	in.SourceFormat = "openai-response"
	in.Metadata["caller_scope"] = "caller-B"
	other := resolve(t, r, c, in)
	if allocate(t, other, "secret") == marker || other.Lease.Restore(marker) != marker {
		t.Fatal("caller isolation failed")
	}
	in.RequestID = "d"
	in.SourceFormat = "gemini"
	in.Headers = http.Header{"X-Session": {"same"}}
	in.RequestedModel = "upstream/model"
	missing := resolve(t, r, c, in)
	if missing.Lease.Persistent() {
		t.Fatal("missing source moved to another profile")
	}
	c.Session.OnMissing = "reject"
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("on_missing reject ignored")
	}
}

func TestHistoryBindingAndConflicts(t *testing.T) {
	store := token.NewStore()
	defer store.Close()
	r := New(store)
	c := cfg(t, sessionConfig)
	in := input("initial")
	in.Headers = http.Header{"X-Session": {"session-1"}}
	a := resolve(t, r, c, in)
	marker := allocate(t, a, "secret")
	ref := protocol.Reference{Kind: "response", ID: "response-1"}
	if err := r.Record(a, []protocol.Reference{ref}); err != nil {
		t.Fatal(err)
	}
	a.Lease.Release()
	in = input("continue")
	in.Previous = []protocol.Reference{ref}
	b := resolve(t, r, c, in)
	if b.Lease.Restore(marker) != "secret" || allocate(t, b, "secret") != marker {
		t.Fatal("history did not recover session")
	}
	in.RequestID = "conflict"
	in.Headers = http.Header{"X-Session": {"different"}}
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("conflicting explicit session accepted")
	}
	in = input("foreign")
	in.Metadata["caller_scope"] = "caller-B"
	in.Previous = []protocol.Reference{ref}
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("foreign caller used reference")
	}
	in = input("unknown")
	in.Previous = []protocol.Reference{{Kind: "response", ID: "missing"}}
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("unknown reference downgraded")
	}
	in = input("other-kind")
	in.Previous = []protocol.Reference{{Kind: "interaction", ID: "response-1"}}
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("history reference kind ignored")
	}
}

func TestExpiredReferenceDoesNotRebindRecreatedSession(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := token.NewStoreWithClock(func() time.Time { return now })
	defer store.Close()
	r := New(store)
	c := cfg(t, sessionConfig)
	c.Session.TTL = config.Duration(time.Minute)
	in := input("first")
	in.Headers = http.Header{"X-Session": {"same-id"}}
	a := resolve(t, r, c, in)
	marker := allocate(t, a, "secret")
	ref := protocol.Reference{Kind: "response", ID: "old-response"}
	if err := r.Record(a, []protocol.Reference{ref}); err != nil {
		t.Fatal(err)
	}
	a.Lease.Release()
	now = now.Add(2 * time.Minute)
	in.RequestID = "new-generation"
	b := resolve(t, r, c, in)
	if allocate(t, b, "secret") == marker {
		t.Fatal("expired mapping retained")
	}
	in = input("old-reference")
	in.Previous = []protocol.Reference{ref}
	if _, err := r.Resolve(c, in); err == nil {
		t.Fatal("old response reference rebound to a new scope generation")
	}
	if r.Stats().References != 0 {
		t.Fatal("expired reference not pruned")
	}
}

func TestReferenceCapacityAndTransactionalRegistration(t *testing.T) {
	store := token.NewStore()
	defer store.Close()
	r := New(store)
	c := cfg(t, sessionConfig)
	in := input("many")
	in.Headers = http.Header{"X-Session": {"same"}}
	s := resolve(t, r, c, in)
	refs := make([]protocol.Reference, MaxReferences+1)
	for i := range refs {
		refs[i] = protocol.Reference{Kind: "response", ID: fmt.Sprintf("r-%d", i)}
	}
	if err := r.Record(s, refs); err == nil {
		t.Fatal("reference capacity ignored")
	}
	if r.Stats().References != 0 {
		t.Fatal("partial reference batch published")
	}
	if err := r.Record(s, refs[:MaxReferences]); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(s, refs[MaxReferences:]); err == nil {
		t.Fatal("reference capacity ignored")
	}
	if stats := r.Stats(); stats.References != MaxReferences || stats.Bytes > MaxReferenceBytes {
		t.Fatal(stats)
	}
	r.Close()
	if r.Stats().References != 0 {
		t.Fatal("close leaked reference bindings")
	}
}
