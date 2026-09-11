// Package plugin implements the request lifecycle independently of the C ABI.
package plugin

import (
	"errors"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
	"github.com/kkkykin/cpa-vibeguard/internal/rules"
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
	mu        sync.Mutex
	config    *snapshot
	decision  decision
	format    string
	body      []byte
	rejection pluginapi.RequestInterceptResponse
	closed    bool
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
}

func New() *Engine { return &Engine{requests: make(map[string]*requestState)} }

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
	s := &requestState{config: snap, format: req.SourceFormat}
	s.mu.Lock()
	e.requests[req.RequestID] = s
	e.mu.Unlock()
	defer s.mu.Unlock()
	if snap.config.Bypass.Matches(req.Headers) {
		s.decision = headerBypass
		e.metrics.header.Add(1)
		return pluginapi.RequestInterceptResponse{}
	}
	return e.prepare(s, req)
}

// prepare is extended by the protocol and rule engine. Until an adapter is
// implemented its entire operation follows the explicit unknown API policy.
func (e *Engine) prepare(s *requestState, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	if s.config.config.OnUnknownAPI == "passthrough" {
		s.decision = unknownPassthrough
		e.metrics.unknown.Add(1)
		return pluginapi.RequestInterceptResponse{}
	}
	s.decision = rejected
	s.rejection = termination(400, "unsupported_api")
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
	// The host already committed the protected body. Credential switches and
	// later interceptors must not rebind a session or append another hint.
	return pluginapi.RequestInterceptResponse{}
}
func (e *Engine) Response(req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	return pluginapi.ResponseInterceptResponse{}
}
func (e *Engine) Stream(req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	return pluginapi.StreamChunkInterceptResponse{}
}
func (e *Engine) Complete(id string) {
	e.mu.Lock()
	s := e.requests[id]
	delete(e.requests, id)
	e.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.body = nil
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
		s.mu.Lock()
		s.closed = true
		s.body = nil
		s.mu.Unlock()
	}
}
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	n := len(e.requests)
	e.mu.Unlock()
	return Stats{Inflight: n, HeaderBypass: e.metrics.header.Load(), UnknownPassthrough: e.metrics.unknown.Load(), RequestErrorPassthrough: e.metrics.requestError.Load(), Rejected: e.metrics.rejected.Load(), Protected: e.metrics.protected.Load(), ResponseDegraded: e.metrics.degraded.Load()}
}
