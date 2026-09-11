package token

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func acquire(t *testing.T, s *Store, key string, persistent bool, limits Limits) *Lease {
	t.Helper()
	l, err := s.Acquire(key, persistent, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	return l
}
func mark(t *testing.T, l *Lease, secret string) string {
	t.Helper()
	tx, err := l.Begin()
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

func TestMarkerFormatStabilityAndIsolation(t *testing.T) {
	s := NewStore()
	defer s.Close()
	a := acquire(t, s, "a", true, Defaults())
	b := acquire(t, s, "b", true, Defaults())
	m := mark(t, a, "中文 secret")
	if !Valid(m) || len(m) != 48 || mark(t, a, "中文 secret") != m {
		t.Fatal("unstable or invalid marker")
	}
	if mark(t, a, "中文 Secret") == m || mark(t, b, "中文 secret") == m {
		t.Fatal("scopes or bytes normalized")
	}
	if got := a.Restore(m + " " + m); got != "中文 secret 中文 secret" {
		t.Fatal(got)
	}
	if got := b.Restore(m); got != m {
		t.Fatal("cross-scope restoration")
	}
	for _, v := range []string{m[14:46], m[:47], strings.ToUpper(m), strings.Replace(m, "v1:", "v1: ", 1)} {
		if got := a.Restore(v); got != v {
			t.Fatal("guessed a malformed marker")
		}
	}
	nested := mark(t, a, "original containing "+m)
	if got := a.Restore(nested); got != "original containing "+m {
		t.Fatal("recursive restoration")
	}
}

func TestConcurrentAllocationAndRollback(t *testing.T) {
	s := NewStore()
	defer s.Close()
	l := acquire(t, s, "session", true, Defaults())
	var wg sync.WaitGroup
	out := make(chan string, 50)
	for i := 0; i < 50; i++ {
		wg.Go(func() {
			tx, err := l.Begin()
			if err != nil {
				t.Error(err)
				return
			}
			m, err := tx.Marker("same secret")
			if err != nil {
				t.Error(err)
				return
			}
			out <- m
			if err := tx.Commit(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(out)
	expected := ""
	for m := range out {
		if expected == "" {
			expected = m
		}
		if m != expected {
			t.Fatal("concurrent allocation was not stable")
		}
	}
	if stats := s.Stats(); stats.Entries != 1 {
		t.Fatal(stats)
	}
	a, _ := l.Begin()
	b, _ := l.Begin()
	m, _ := a.Marker("pending shared")
	m2, _ := b.Marker("pending shared")
	if m != m2 {
		t.Fatal("pending allocations differ")
	}
	if _, ok := l.Lookup(m); ok {
		t.Fatal("uncommitted mapping visible to restoration")
	}
	a.Rollback()
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if v, ok := l.Lookup(m); !ok || v != "pending shared" {
		t.Fatal("rollback deleted another request's committed mapping")
	}
	c, _ := l.Begin()
	unpublished, _ := c.Marker("unpublished")
	c.Rollback()
	c.Rollback()
	if _, ok := l.Lookup(unpublished); ok || s.Stats().Entries != 2 {
		t.Fatal("temporary mapping retained")
	}
}

func TestLeaseTTLAndCapacity(t *testing.T) {
	s := NewStore()
	defer s.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	limits := Defaults()
	limits.MaxSessions = 1
	limits.MaxScopes = 2
	limits.MaxEntries = 1
	limits.TTL = time.Minute
	l := acquire(t, s, "session", true, limits)
	m := mark(t, l, "secret")
	now = now.Add(2 * time.Minute)
	if _, err := s.Acquire("other", true, limits); err == nil {
		t.Fatal("active lease evicted for capacity")
	}
	if l.Restore(m) != "secret" {
		t.Fatal("TTL evicted active lease")
	}
	tx, _ := l.Begin()
	if _, err := tx.Marker("second"); err == nil {
		t.Fatal("per-scope capacity ignored")
	}
	tx.Rollback()
	l.Release()
	l.Release()
	now = now.Add(2 * time.Minute)
	if _, err := s.AcquireExisting("session", limits); err == nil {
		t.Fatal("expired session silently recreated")
	}
	if stats := s.Stats(); stats.Entries != 0 || stats.Bytes != 0 {
		t.Fatal(stats)
	}
	other := acquire(t, s, "other", true, limits)
	mark(t, other, "x")
	other.Release()
	third := acquire(t, s, "third", true, limits)
	if stats := s.Stats(); stats.Sessions != 1 || stats.Entries != 0 {
		t.Fatal(stats)
	}
	third.Release()
}

func TestRequestScopeAndReleaseRollback(t *testing.T) {
	s := NewStore()
	defer s.Close()
	l := acquire(t, s, "request", false, Defaults())
	mark(t, l, "committed")
	tx, _ := l.Begin()
	tx.Marker("pending")
	l.Release()
	tx.Rollback()
	if stats := s.Stats(); stats.Entries != 0 || stats.Scopes != 0 || stats.Bytes != 0 || stats.Leases != 0 {
		t.Fatal(stats)
	}
	if _, err := tx.Marker("late"); err == nil {
		t.Fatal("closed transaction usable")
	}
}

func TestGlobalByteLimitCollisionAndClose(t *testing.T) {
	s := NewStore()
	s.random = bytes.NewReader(make([]byte, 16*20))
	limits := Defaults()
	limits.MaxBytes = 1024
	l := acquire(t, s, "scope", true, limits)
	mark(t, l, "first")
	tx, _ := l.Begin()
	if _, err := tx.Marker("collision"); err == nil || err.Error() != "marker_collision" {
		t.Fatal(err)
	}
	tx.Rollback()
	tx, _ = l.Begin()
	if _, err := tx.Marker(strings.Repeat("x", 1024)); err == nil {
		t.Fatal("byte limit ignored")
	}
	tx.Rollback()
	s.Close()
	s.Close()
	if stats := s.Stats(); stats != (Stats{}) {
		t.Fatal(stats)
	}
	if _, err := s.Acquire("closed", true, limits); err == nil {
		t.Fatal("closed store admitted a lease")
	}
}

func TestPossibleMarkerPrefixes(t *testing.T) {
	marker := Prefix + fmt.Sprintf("%032x", 42) + "]]"
	for i := 0; i <= len(marker); i++ {
		if !Possible(marker[:i]) {
			t.Fatalf("valid prefix of length %d rejected", i)
		}
	}
	for _, v := range []string{"[x", Prefix + "G", Prefix + strings.Repeat("a", 32) + "x", marker + "x"} {
		if Possible(v) {
			t.Fatalf("invalid candidate %q", v)
		}
	}
}
