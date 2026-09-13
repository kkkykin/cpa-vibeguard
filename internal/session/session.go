// Package session binds configured session sources and continuation references
// to authenticated caller scopes. A reference never creates a missing mapping.
package session

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
	"github.com/kkkykin/cpa-vibeguard/internal/document"
	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

const MaxReferences = 8192
const MaxReferenceBytes = 8 << 20
const MaxCallerReferences = 1024
const MaxCallerReferenceBytes = 1 << 20

// ErrReferenceCapacity means a validated batch cannot fit in the history
// index. It does not invalidate the current request's leased secret mapping.
var ErrReferenceCapacity = errors.New("history_reference_capacity")

type Input struct {
	RequestID, SourceFormat, RequestedModel string
	Headers                                 http.Header
	JSON                                    any
	Metadata                                map[string]any
	Previous                                []protocol.Reference
}
type Selection struct {
	Lease             *token.Lease
	Namespace, Caller string
	MaxIDBytes        int
}
type referenceKey struct{ caller, namespace, kind, id string }
type lookupKey struct{ caller, kind, id string }

func (k referenceKey) lookup() lookupKey { return lookupKey{k.caller, k.kind, k.id} }

type binding struct {
	key                  string
	generation           uint64
	bytes                int
	recent, callerRecent *list.Element
}
type callerReferences struct {
	recent list.List
	bytes  int
}
type Resolver struct {
	mu      sync.Mutex
	store   *token.Store
	refs    map[lookupKey]map[string]*binding // lookup key -> namespace -> binding
	recent  list.List
	callers map[string]*callerReferences
	count   int
	bytes   int
}
type Stats struct{ References, Bytes int }

func New(store *token.Store) *Resolver {
	return &Resolver{store: store, refs: make(map[lookupKey]map[string]*binding), callers: make(map[string]*callerReferences)}
}

func validID(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func scopeKey(namespace, caller, id string) string {
	var b strings.Builder
	b.WriteString("session:")
	var n [8]byte
	for _, s := range []string{namespace, caller, id} {
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		b.Write(n[:])
		b.WriteString(s)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "session:" + hex.EncodeToString(sum[:])
}
func limits(cfg *config.Config) token.Limits {
	l := token.Defaults()
	if s := cfg.Session; s != nil {
		l.TTL = time.Duration(s.TTL)
		l.MaxSessions = s.MaxSessions
		l.MaxEntries = s.MaxEntriesPerSession
		l.MaxBytes = s.MaxTotalBytes
	}
	l.MaxScopes = l.MaxSessions + cfg.Limits.MaxInflight
	return l
}
func choose(s *config.Session, in Input) *config.Profile {
	for i := range s.Profiles {
		p := &s.Profiles[i]
		if p.Match.SourceFormat != "" && p.Match.SourceFormat != in.SourceFormat {
			continue
		}
		if p.Match.RequestedModelPrefix != "" && !strings.HasPrefix(in.RequestedModel, p.Match.RequestedModelPrefix) {
			continue
		}
		return p
	}
	return nil
}
func candidate(p *config.Profile, in Input, max int) (string, error) {
	for _, s := range p.Sources {
		var v any
		var exists bool
		var err error
		switch {
		case s.Header != nil:
			values, present := config.HeaderValues(in.Headers, *s.Header)
			if present && len(values) != 1 {
				return "", errors.New("session_header_ambiguous")
			}
			if len(values) == 1 {
				v = values[0]
				exists = true
			}
		case s.JSON != nil:
			v, exists, err = document.Pointer(in.JSON, *s.JSON)
		case s.Metadata != nil:
			v, exists, err = document.Pointer(in.Metadata, *s.Metadata)
		default:
			return "", errors.New("invalid_session_source")
		}
		if err != nil {
			return "", errors.New("invalid_session_source")
		}
		if !exists || v == nil {
			continue
		}
		value, ok := v.(string)
		if !ok {
			return "", errors.New("session_id_type")
		}
		if value == "" {
			continue
		}
		if !validID(value, max) {
			return "", errors.New("invalid_session_id")
		}
		return value, nil
	}
	return "", nil
}

func (r *Resolver) Resolve(cfg *config.Config, in Input) (Selection, error) {
	l := limits(cfg)
	request := func() (Selection, error) {
		lease, err := r.store.Acquire("request:"+in.RequestID, false, l)
		return Selection{Lease: lease}, err
	}
	if cfg.Session == nil {
		if len(in.Previous) > 0 {
			return Selection{}, errors.New("history_requires_session")
		}
		return request()
	}
	s := cfg.Session
	p := choose(s, in)
	var sessionID, namespace string
	if p != nil {
		var err error
		sessionID, err = candidate(p, in, s.MaxIDBytes)
		if err != nil {
			return Selection{}, err
		}
		namespace = p.Namespace
	}
	caller, _ := in.Metadata["caller_scope"].(string)
	if sessionID != "" || len(in.Previous) > 0 {
		if !validID(caller, 1024) {
			return Selection{}, errors.New("trusted_caller_scope_required")
		}
	}
	key := ""
	if sessionID != "" {
		key = scopeKey(namespace, caller, sessionID)
	}
	if len(in.Previous) > 0 {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.prune()
		var selected *binding
		var used []*binding
		found := false
		for _, ref := range in.Previous {
			if !validID(ref.ID, s.MaxIDBytes) {
				return Selection{}, errors.New("invalid_history_reference")
			}
			var b *binding
			var ns string
			namespaces := r.refs[lookupKey{caller, ref.Kind, ref.ID}]
			if p != nil {
				var exists bool
				b, exists = namespaces[namespace]
				if !exists {
					return Selection{}, errors.New("unknown_or_ambiguous_history_reference")
				}
				ns = namespace
			} else {
				if len(namespaces) != 1 {
					return Selection{}, errors.New("unknown_or_ambiguous_history_reference")
				}
				for name, value := range namespaces {
					ns, b = name, value
				}
			}
			if (key != "" && key != b.key) || (found && (selected.key != b.key || selected.generation != b.generation)) {
				return Selection{}, errors.New("history_session_conflict")
			}
			selected = b
			used = append(used, b)
			namespace = ns
			found = true
		}
		lease, err := r.store.AcquireGeneration(selected.key, selected.generation, l)
		if err != nil {
			return Selection{}, err
		}
		// Failed or conflicting history requests must not refresh retention.
		for _, b := range used {
			r.touch(caller, b)
		}
		return Selection{Lease: lease, Namespace: namespace, Caller: caller, MaxIDBytes: s.MaxIDBytes}, nil
	}
	if sessionID == "" {
		if p != nil && s.OnMissing == "reject" {
			return Selection{}, errors.New("session_source_missing")
		}
		return request()
	}
	lease, err := r.store.Acquire(key, true, l)
	return Selection{Lease: lease, Namespace: namespace, Caller: caller, MaxIDBytes: s.MaxIDBytes}, err
}

// Record validates the entire batch before publishing or evicting bindings.
// Valid batches refresh LRU retention; both global and per-caller limits apply
// to references independently of secret mapping leases. The scope generation
// prevents old references from rebinding after a session ID is reused.
func (r *Resolver) Record(selected Selection, refs []protocol.Reference) error {
	if selected.Lease == nil || !selected.Lease.Persistent() || len(refs) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	batch := make(map[referenceKey]*binding)
	order := make([]referenceKey, 0, min(len(refs), MaxCallerReferences))
	batchBytes, overCapacity := 0, false
	scope, generation := selected.Lease.Key(), selected.Lease.Generation()
	for _, ref := range refs {
		if !validID(ref.ID, selected.MaxIDBytes) {
			return errors.New("invalid_response_reference")
		}
		k := referenceKey{caller: selected.Caller, namespace: selected.Namespace, kind: ref.Kind, id: ref.ID}
		b, exists := r.refs[k.lookup()][k.namespace]
		if exists {
			if b.key != scope || b.generation != generation {
				return errors.New("response_reference_conflict")
			}
		}
		// Keep staging bounded, but still validate the rest of an oversized
		// batch so capacity cannot hide malformed IDs or binding conflicts.
		if overCapacity {
			continue
		}
		if _, exists := batch[k]; exists {
			continue
		}
		if b == nil {
			b = &binding{key: scope, generation: generation}
			// Charge both map levels, both LRU nodes, and caller accounting,
			// even when those structures share strings or namespace maps.
			b.bytes = len(k.caller) + len(k.namespace) + len(k.kind) + len(k.id) + len(b.key) + 896
		}
		if len(batch) >= MaxCallerReferences || b.bytes > MaxCallerReferenceBytes-batchBytes {
			overCapacity = true
			continue
		}
		batch[k] = b
		order = append(order, k)
		batchBytes += b.bytes
	}
	if overCapacity {
		return ErrReferenceCapacity
	}
	for _, k := range order {
		b := batch[k]
		if b.recent != nil {
			r.touch(k.caller, b)
			continue
		}
		caller := r.callers[k.caller]
		if caller == nil {
			caller = &callerReferences{}
			r.callers[k.caller] = caller
		}
		b.recent = r.recent.PushBack(k)
		b.callerRecent = caller.recent.PushBack(b.recent)
		caller.bytes += b.bytes
		key := k.lookup()
		if r.refs[key] == nil {
			r.refs[key] = make(map[string]*binding)
		}
		r.refs[key][k.namespace] = b
		r.count++
		r.bytes += b.bytes
	}
	// Every batch member is now newer than all eviction candidates, and the
	// batch itself fits both budgets. Eviction cannot remove a batch member.
	caller := r.callers[selected.Caller]
	for caller.recent.Len() > MaxCallerReferences || caller.bytes > MaxCallerReferenceBytes {
		oldest := caller.recent.Front().Value.(*list.Element)
		r.remove(oldest.Value.(referenceKey))
	}
	for r.count > MaxReferences || r.bytes > MaxReferenceBytes {
		r.remove(r.recent.Front().Value.(referenceKey))
	}
	return nil
}

func (r *Resolver) touch(caller string, b *binding) {
	r.recent.MoveToBack(b.recent)
	r.callers[caller].recent.MoveToBack(b.callerRecent)
}

func (r *Resolver) remove(k referenceKey) {
	key := k.lookup()
	namespaces := r.refs[key]
	b := namespaces[k.namespace]
	delete(namespaces, k.namespace)
	if len(namespaces) == 0 {
		delete(r.refs, key)
	}
	r.recent.Remove(b.recent)
	caller := r.callers[k.caller]
	caller.recent.Remove(b.callerRecent)
	caller.bytes -= b.bytes
	if caller.recent.Len() == 0 {
		delete(r.callers, k.caller)
	}
	r.count--
	r.bytes -= b.bytes
}

func (r *Resolver) prune() {
	if r.count == 0 {
		return
	}
	live := r.store.LiveGenerations()
	for key, namespaces := range r.refs {
		for namespace, b := range namespaces {
			if generation, exists := live[b.key]; !exists || generation != b.generation {
				r.remove(referenceKey{caller: key.caller, namespace: namespace, kind: key.kind, id: key.id})
			}
		}
	}
}
func (r *Resolver) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	return Stats{References: r.count, Bytes: r.bytes}
}
func (r *Resolver) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = make(map[lookupKey]map[string]*binding)
	r.recent.Init()
	r.callers = make(map[string]*callerReferences)
	r.count = 0
	r.bytes = 0
}
