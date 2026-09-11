// Package plugin implements the request lifecycle independently of the C ABI.
package plugin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/restore"
	"github.com/kkkykin/cpa-vibeguard/internal/rules"
	"github.com/kkkykin/cpa-vibeguard/internal/session"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type decision uint8

const (
	protected decision = iota
	headerBypass
	unknownPassthrough
	errorPassthrough
	rejected
)

type snapshot struct {
	config     *config.Config
	generation uint64
	rules      *rules.Engine
}
type requestState struct {
	mu            sync.Mutex
	config        *snapshot
	decision      decision
	format        string
	body          []byte
	rejection     pluginapi.RequestInterceptResponse
	closed        bool
	lease         *token.Lease
	degraded      bool
	ctx           context.Context
	cancel        context.CancelFunc
	selection     session.Selection
	stream        *restore.Stream
	streamStopped bool
}
type counters struct{ header, unknown, requestError, rejected, protected, degraded atomic.Uint64 }
type Stats struct {
	Inflight                                                                                         int
	HeaderBypass, UnknownPassthrough, RequestErrorPassthrough, Rejected, Protected, ResponseDegraded uint64
}

type Engine struct {
	mu       sync.Mutex
	current  *snapshot
	requests map[string]*requestState
	quiesced bool
	closed   bool
	metrics  counters
	mappings *token.Store
	sessions *session.Resolver
}

func New() *Engine {
	store := token.NewStore()
	return &Engine{requests: make(map[string]*requestState), mappings: store, sessions: session.New(store)}
}

// Configure builds a complete candidate before publishing. In-flight requests
// keep the snapshot captured by their first before-auth call.
func (e *Engine) Configure(raw []byte) error {
	cfg, err := config.Parse(raw)
	if err != nil {
		return err
	}
	ruleEngine, err := rules.New(cfg, nil)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("plugin_closed")
	}
	generation := uint64(1)
	if e.current != nil {
		generation = e.current.generation + 1
	}
	e.current = &snapshot{config: cfg, generation: generation, rules: ruleEngine}
	// CPA also uses reconfigure to reactivate the old instance when a binary
	// replacement fails after quiesce. A closed instance cannot be resumed.
	e.quiesced = false
	return nil
}

func termination(status int, code string) pluginapi.RequestInterceptResponse {
	// Codes are internal constants; never include a body, variable, or identity.
	return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: status,
		ResponseHeaders: http.Header{"Content-Type": {"application/json"}},
		ResponseBody:    []byte(`{"error":{"type":"vibeguard_request_rejected","code":"` + code + `"}}`)}
}

func (e *Engine) Before(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	if req.RequestID == "" || len(req.RequestID) > 256 {
		return termination(400, "invalid_request_id")
	}
	e.mu.Lock()
	if s := e.requests[req.RequestID]; s != nil {
		e.mu.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.requestResponse()
	}
	if e.closed || e.quiesced || e.current == nil {
		e.mu.Unlock()
		return termination(503, "plugin_unavailable")
	}
	snap := e.current
	if len(e.requests) >= snap.config.Limits.MaxInflight {
		e.mu.Unlock()
		e.metrics.rejected.Add(1)
		return termination(429, "inflight_limit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &requestState{config: snap, format: req.SourceFormat, ctx: ctx, cancel: cancel}
	s.mu.Lock()
	e.requests[req.RequestID] = s
	e.mu.Unlock()
	defer s.mu.Unlock()
	if snap.config.Bypass.Matches(req.Headers) {
		s.decision = headerBypass
		s.config = nil
		s.format = ""
		e.metrics.header.Add(1)
		return pluginapi.RequestInterceptResponse{}
	}
	return e.prepare(s, req)
}

func (e *Engine) prepare(s *requestState, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	if !protocol.Known(req.SourceFormat) {
		return e.unknown(s)
	}
	cfg := s.config.config
	if len(req.Body) > cfg.Limits.RequestBytes {
		return e.requestError(s, "request_size_limit")
	}
	doc, err := protocol.ParseRequest(req.SourceFormat, req.Body, req.Headers, cfg.Limits.ToolJSONBytes)
	if err != nil {
		if errors.Is(err, protocol.ErrUnknown) {
			return e.unknown(s)
		}
		return e.requestError(s, "invalid_request_body")
	}
	bound, err := s.config.rules.Bind(rules.Input{Headers: req.Headers, JSON: doc.Root, Metadata: req.Metadata})
	if err != nil {
		return e.requestError(s, "variable_binding_failed")
	}
	selection, err := e.sessions.Resolve(cfg, session.Input{RequestID: req.RequestID, SourceFormat: req.SourceFormat, RequestedModel: req.RequestedModel, Headers: req.Headers, JSON: doc.Root, Metadata: req.Metadata, Previous: doc.Previous})
	if err != nil {
		return e.requestError(s, "session_resolution_failed")
	}
	lease := selection.Lease
	committed := false
	defer func() {
		if !committed {
			lease.Release()
		}
	}()
	tx, err := lease.Begin()
	if err != nil {
		return e.requestError(s, "mapping_transaction_failed")
	}
	defer tx.Rollback()
	expandedBytes := len(req.Body)
	for _, field := range doc.Fields {
		spans, err := bound.Scan(s.ctx, field.Text)
		if err != nil {
			return e.requestError(s, "rule_scan_failed")
		}
		if len(spans) == 0 {
			continue
		}
		var out strings.Builder
		previous := 0
		for _, span := range spans {
			expandedBytes += token.Length - (span.End - span.Start)
			if expandedBytes > cfg.Limits.RequestBytes || expandedBytes > cfg.Limits.RequestStateBytes {
				return e.requestError(s, "request_state_limit")
			}
			marker, err := tx.Marker(field.Text[span.Start:span.End])
			if err != nil {
				return e.requestError(s, "mapping_allocation_failed")
			}
			out.WriteString(field.Text[previous:span.Start])
			out.WriteString(marker)
			previous = span.End
		}
		out.WriteString(field.Text[previous:])
		field.Set(out.String())
	}
	if cfg.PreserveMarkerHint {
		doc.AppendHint()
	}
	body, err := doc.Marshal()
	if err != nil {
		return e.requestError(s, "request_encode_failed")
	}
	if len(body) > cfg.Limits.RequestBytes || len(body) > cfg.Limits.RequestStateBytes {
		return e.requestError(s, "request_state_limit")
	}
	if err := tx.Commit(); err != nil {
		return e.requestError(s, "mapping_commit_failed")
	}
	s.body = body
	s.lease = lease
	s.selection = selection
	s.decision = protected
	committed = true
	e.metrics.protected.Add(1)
	return s.requestResponse()
}

func (e *Engine) unknown(s *requestState) pluginapi.RequestInterceptResponse {
	if s.config.config.OnUnknownAPI == "passthrough" {
		s.decision = unknownPassthrough
		s.config = nil
		s.format = ""
		e.metrics.unknown.Add(1)
		return pluginapi.RequestInterceptResponse{}
	}
	s.decision = rejected
	s.rejection = termination(400, "unsupported_api")
	s.config = nil
	e.metrics.rejected.Add(1)
	return s.rejection
}

func (e *Engine) requestError(s *requestState, code string) pluginapi.RequestInterceptResponse {
	if s.config.config.OnRequestError == "passthrough" {
		s.decision = errorPassthrough
		s.config = nil
		e.metrics.requestError.Add(1)
		return pluginapi.RequestInterceptResponse{}
	}
	s.decision = rejected
	s.rejection = termination(400, code)
	s.config = nil
	e.metrics.rejected.Add(1)
	return s.rejection
}

func (s *requestState) requestResponse() pluginapi.RequestInterceptResponse {
	if s.closed {
		return termination(409, "request_completed")
	}
	if s.decision == rejected {
		return s.rejection
	}
	if s.decision == protected {
		return pluginapi.RequestInterceptResponse{Body: append([]byte(nil), s.body...)}
	}
	return pluginapi.RequestInterceptResponse{}
}

func (e *Engine) state(id string) *requestState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests[id]
}
func (e *Engine) After(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	s := e.state(req.RequestID)
	if s == nil {
		return termination(409, "request_state_missing")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decision == rejected || s.closed {
		return s.requestResponse()
	}
	if s.decision == protected && req.SourceFormat != s.format {
		return termination(409, "request_format_changed")
	}
	// The host already committed the protected body. Credential switches and
	// later interceptors must not rebind a session or append another hint.
	return pluginapi.RequestInterceptResponse{}
}
func (e *Engine) Response(req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	s := e.state(req.RequestID)
	if s == nil {
		return pluginapi.ResponseInterceptResponse{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.decision != protected {
		return pluginapi.ResponseInterceptResponse{}
	}
	fail := func() pluginapi.ResponseInterceptResponse { e.degrade(s); return pluginapi.ResponseInterceptResponse{} }
	cfg := s.config.config
	if req.SourceFormat != s.format || len(req.Body) > cfg.Limits.ResponseBytes {
		return fail()
	}
	doc, err := protocol.Parse(req.SourceFormat, req.Body, false, cfg.Limits.ToolJSONBytes)
	if err != nil {
		return fail()
	}
	remaining := cfg.Limits.ResponseBytes
	for _, field := range doc.Fields {
		text, err := token.RewriteBounded(field.Text, s.lease.Lookup, remaining)
		if err != nil {
			return fail()
		}
		remaining -= len(text)
		field.Set(text)
	}
	body, err := doc.Marshal()
	if err != nil || len(body) > cfg.Limits.ResponseBytes {
		return fail()
	}
	if err := e.sessions.Record(s.selection, doc.ResponseIDs); err != nil {
		return fail()
	}
	return pluginapi.ResponseInterceptResponse{Body: body}
}
func (e *Engine) degrade(s *requestState) {
	if !s.degraded {
		s.degraded = true
		e.metrics.degraded.Add(1)
	}
}
func (e *Engine) Stream(req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	s := e.state(req.RequestID)
	if s == nil {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.decision != protected || s.streamStopped {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	fail := func() pluginapi.StreamChunkInterceptResponse {
		if s.stream != nil {
			s.stream.Abort()
			s.stream = nil
		}
		s.streamStopped = true
		e.degrade(s)
		return pluginapi.StreamChunkInterceptResponse{}
	}
	if req.SourceFormat != s.format {
		return fail()
	}
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex || len(req.Body) == 0 {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	if s.stream == nil {
		limits := s.config.config.Limits
		limits.RequestStateBytes -= len(s.body)
		if limits.RequestStateBytes <= 0 {
			return fail()
		}
		s.stream = restore.NewStream(s.format, limits, s.lease.Lookup)
	}
	body, ids, err := s.stream.Push(req.Body)
	if err != nil {
		return fail()
	}
	if err := e.sessions.Record(s.selection, ids); err != nil {
		return fail()
	}
	return pluginapi.StreamChunkInterceptResponse{Body: body, DropChunk: len(body) == 0}
}
func (e *Engine) Complete(id string) {
	e.mu.Lock()
	s := e.requests[id]
	delete(e.requests, id)
	e.mu.Unlock()
	if s == nil {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.body = nil
	if s.stream != nil {
		s.stream.Abort()
		s.stream = nil
	}
	if s.lease != nil {
		s.lease.Release()
		s.lease = nil
	}
	s.selection = session.Selection{}
}
func (e *Engine) Quiesce() { e.mu.Lock(); defer e.mu.Unlock(); e.quiesced = true }
func (e *Engine) Close() {
	e.mu.Lock()
	e.closed = true
	states := e.requests
	e.requests = make(map[string]*requestState)
	e.current = nil
	e.mu.Unlock()
	for _, s := range states {
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Lock()
		s.closed = true
		s.body = nil
		if s.stream != nil {
			s.stream.Abort()
			s.stream = nil
		}
		if s.lease != nil {
			s.lease.Release()
			s.lease = nil
		}
		s.selection = session.Selection{}
		s.mu.Unlock()
	}
	e.mappings.Close()
	e.sessions.Close()
}
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	n := len(e.requests)
	e.mu.Unlock()
	return Stats{Inflight: n, HeaderBypass: e.metrics.header.Load(), UnknownPassthrough: e.metrics.unknown.Load(), RequestErrorPassthrough: e.metrics.requestError.Load(), Rejected: e.metrics.rejected.Load(), Protected: e.metrics.protected.Load(), ResponseDegraded: e.metrics.degraded.Load()}
}
