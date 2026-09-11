package token

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Limits struct {
	TTL                                          time.Duration
	MaxSessions, MaxScopes, MaxEntries, MaxBytes int
}

func Defaults() Limits {
	return Limits{TTL: 30 * time.Minute, MaxSessions: 1024, MaxScopes: 1280, MaxEntries: 4096, MaxBytes: 64 << 20}
}

type entry struct {
	secret, marker string
	committed      bool
	holds          int
	bytes          int
}
type scope struct {
	key        string
	generation uint64
	persistent bool
	bySecret   map[string]*entry
	byMarker   map[string]*entry
	leases     map[*Lease]bool
	used       time.Time
	ttl        time.Duration
	bytes      int
}
type Store struct {
	mu             sync.Mutex
	scopes         map[string]*scope
	markers        map[string]bool
	bytes          int
	closed         bool
	now            func() time.Time
	random         io.Reader
	nextGeneration uint64
}
type Lease struct {
	store        *Store
	scope        *scope
	limits       Limits
	released     bool
	transactions map[*Transaction]bool
}
type Transaction struct {
	lease  *Lease
	staged map[*entry]bool
	closed bool
}
type Stats struct{ Scopes, Sessions, Entries, Bytes, Leases int }

func NewStore() *Store {
	return &Store{scopes: make(map[string]*scope), markers: make(map[string]bool), now: time.Now, random: rand.Reader}
}
func NewStoreWithClock(now func() time.Time) *Store {
	s := NewStore()
	if now != nil {
		s.now = now
	}
	return s
}

func (s *Store) Acquire(key string, persistent bool, limits Limits) (*Lease, error) {
	return s.acquire(key, persistent, limits, false, 0)
}
func (s *Store) AcquireExisting(key string, limits Limits) (*Lease, error) {
	return s.acquire(key, true, limits, true, 0)
}
func (s *Store) AcquireGeneration(key string, generation uint64, limits Limits) (*Lease, error) {
	return s.acquire(key, true, limits, true, generation)
}
func (s *Store) acquire(key string, persistent bool, limits Limits, existing bool, generation uint64) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("mapping_store_closed")
	}
	if key == "" || len(key) > 4096 || limits.TTL <= 0 || limits.MaxScopes < 1 || limits.MaxSessions < 1 || limits.MaxEntries < 1 || limits.MaxBytes < 1 {
		return nil, errors.New("invalid_mapping_scope")
	}
	s.prune()
	c := s.scopes[key]
	if c == nil {
		if existing {
			return nil, errors.New("session_mapping_expired")
		}
		bytes := 256 + len(key)
		for len(s.scopes) >= limits.MaxScopes || (persistent && s.sessionCount() >= limits.MaxSessions) || s.bytes+bytes > limits.MaxBytes {
			if !s.evict(nil) {
				return nil, errors.New("mapping_capacity")
			}
		}
		s.nextGeneration++
		c = &scope{key: strings.Clone(key), generation: s.nextGeneration, persistent: persistent, bySecret: make(map[string]*entry), byMarker: make(map[string]*entry), leases: make(map[*Lease]bool), used: s.now(), ttl: limits.TTL, bytes: bytes}
		s.scopes[key] = c
		s.bytes += bytes
	} else if c.persistent != persistent {
		return nil, errors.New("mapping_scope_conflict")
	}
	if generation != 0 && c.generation != generation {
		return nil, errors.New("session_mapping_expired")
	}
	l := &Lease{store: s, scope: c, limits: limits, transactions: make(map[*Transaction]bool)}
	c.leases[l] = true
	c.used = s.now()
	c.ttl = limits.TTL
	return l, nil
}

func (s *Store) sessionCount() int {
	n := 0
	for _, c := range s.scopes {
		if c.persistent {
			n++
		}
	}
	return n
}
func (s *Store) prune() {
	now := s.now()
	for _, c := range s.scopes {
		if len(c.leases) == 0 && now.Sub(c.used) >= c.ttl {
			s.remove(c)
		}
	}
}
func (s *Store) evict(except *scope) bool {
	var oldest *scope
	for _, c := range s.scopes {
		if c == except || len(c.leases) > 0 {
			continue
		}
		if oldest == nil || c.used.Before(oldest.used) {
			oldest = c
		}
	}
	if oldest == nil {
		return false
	}
	s.remove(oldest)
	return true
}
func (s *Store) remove(c *scope) {
	for marker := range c.byMarker {
		delete(s.markers, marker)
	}
	s.bytes -= c.bytes
	delete(s.scopes, c.key)
	c.byMarker = nil
	c.bySecret = nil
	c.bytes = 0
}

func (l *Lease) Begin() (*Transaction, error) {
	s := l.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.released || s.closed {
		return nil, errors.New("mapping_lease_released")
	}
	t := &Transaction{lease: l, staged: make(map[*entry]bool)}
	l.transactions[t] = true
	return t, nil
}
func (t *Transaction) Marker(secret string) (string, error) {
	l := t.lease
	s := l.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.closed || l.released || s.closed {
		return "", errors.New("mapping_transaction_closed")
	}
	if secret == "" || !utf8.ValidString(secret) {
		return "", errors.New("invalid_secret_bytes")
	}
	c := l.scope
	if old := c.bySecret[secret]; old != nil {
		if !old.committed && !t.staged[old] {
			old.holds++
			t.staged[old] = true
		}
		return old.marker, nil
	}
	bytes := len(secret) + Length + 192
	if len(c.bySecret) >= l.limits.MaxEntries || bytes > l.limits.MaxBytes {
		return "", errors.New("mapping_capacity")
	}
	s.prune()
	for s.bytes+bytes > l.limits.MaxBytes {
		if !s.evict(c) {
			return "", errors.New("mapping_capacity")
		}
	}
	marker := ""
	for attempt := 0; attempt < 8; attempt++ {
		var id [16]byte
		if _, err := io.ReadFull(s.random, id[:]); err != nil {
			return "", errors.New("marker_random_error")
		}
		candidate := Prefix + hex.EncodeToString(id[:]) + "]]"
		if !s.markers[candidate] {
			marker = candidate
			break
		}
	}
	if marker == "" {
		return "", errors.New("marker_collision")
	}
	e := &entry{secret: strings.Clone(secret), marker: marker, holds: 1, bytes: bytes}
	c.bySecret[e.secret] = e
	c.byMarker[marker] = e
	s.markers[marker] = true
	c.bytes += bytes
	s.bytes += bytes
	t.staged[e] = true
	return marker, nil
}
func (t *Transaction) Commit() error {
	s := t.lease.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.closed || t.lease.released || s.closed {
		return errors.New("mapping_transaction_closed")
	}
	for e := range t.staged {
		e.committed = true
		e.holds--
	}
	t.closed = true
	t.staged = nil
	delete(t.lease.transactions, t)
	return nil
}
func (t *Transaction) Rollback() { s := t.lease.store; s.mu.Lock(); defer s.mu.Unlock(); t.rollback() }
func (t *Transaction) rollback() {
	if t.closed {
		return
	}
	l := t.lease
	s := l.store
	c := l.scope
	for e := range t.staged {
		e.holds--
		if !e.committed && e.holds == 0 {
			delete(c.bySecret, e.secret)
			delete(c.byMarker, e.marker)
			delete(s.markers, e.marker)
			c.bytes -= e.bytes
			s.bytes -= e.bytes
		}
	}
	t.closed = true
	t.staged = nil
	delete(l.transactions, t)
}
func (l *Lease) Lookup(marker string) (string, bool) {
	s := l.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.released || s.closed {
		return "", false
	}
	e := l.scope.byMarker[marker]
	if e == nil || !e.committed {
		return "", false
	}
	return e.secret, true
}
func (l *Lease) Restore(text string) string { return Rewrite(text, l.Lookup) }
func (l *Lease) Key() string                { return l.scope.key }
func (l *Lease) Generation() uint64         { return l.scope.generation }
func (l *Lease) Persistent() bool           { return l.scope.persistent }
func (l *Lease) Release()                   { s := l.store; s.mu.Lock(); defer s.mu.Unlock(); l.release() }
func (l *Lease) release() {
	if l.released {
		return
	}
	for t := range l.transactions {
		t.rollback()
	}
	c := l.scope
	l.released = true
	delete(c.leases, l)
	c.used = l.store.now()
	if !c.persistent && len(c.leases) == 0 {
		l.store.remove(c)
	}
}
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	stats := Stats{Scopes: len(s.scopes), Sessions: s.sessionCount(), Entries: len(s.markers), Bytes: s.bytes}
	for _, c := range s.scopes {
		stats.Leases += len(c.leases)
	}
	return stats
}
func (s *Store) Live(key string, generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	c := s.scopes[key]
	return !s.closed && c != nil && c.generation == generation
}
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for _, c := range s.scopes {
		for l := range c.leases {
			l.release()
		}
		if s.scopes[c.key] != nil {
			s.remove(c)
		}
	}
	s.closed = true
}
