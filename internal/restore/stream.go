package restore

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/kkkykin/cpa-vibeguard/internal/config"
	"github.com/kkkykin/cpa-vibeguard/internal/document"
	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

const MaxStreamFields = 4096

// Stream is owned by one request. The plugin serializes Push and Abort with
// that request's mutex. It retains field delivery coordinates, never events.
type Stream struct {
	format                         string
	limits                         config.Limits
	lookup                         Lookup
	framer                         Framer
	fields                         map[string]*streamField
	closed                         map[string]bool
	aliases                        map[string]string
	aliasBytes                     int
	arrayStarted, arrayItem, ended bool
	budget                         int
	budgetExceeded                 bool
	responseSSE                    bool
	sequenceOffset                 int64
}

type streamField struct {
	group    string
	text     *Text
	tool     *ToolJSON
	delivery delivery
	closed   bool
}

// Only small protocol coordinates survive an event. In particular there are
// no setters or closures retaining a parsed chunk, tool snapshot, or history.
type delivery struct {
	family, field, event, deltaKind, itemID, contentKey string
	index, tool, part                                   int
	hasIndex, thought                                   bool
}
type tail struct {
	delivery delivery
	text     string
}

func NewStream(format string, limits config.Limits, lookup Lookup) *Stream {
	return &Stream{format: format, limits: limits, lookup: lookup,
		framer: Framer{Limit: limits.EventBytes, Records: true}, fields: make(map[string]*streamField),
		closed: make(map[string]bool), aliases: make(map[string]string)}
}

// Buffered includes cached field coordinates and completed-field tombstones.
// The caller subtracts its cached redacted request from RequestStateBytes.
func (s *Stream) Buffered() int {
	n := s.framer.Buffered() + s.aliasBytes
	for k, f := range s.fields {
		n += 512 + len(k) + len(f.group) + len(f.delivery.itemID)
		if f.text != nil {
			n += f.text.Buffered()
		}
		if f.tool != nil {
			n += f.tool.Buffered()
		}
	}
	for k := range s.closed {
		n += 64 + len(k)
	}
	return n
}
func (s *Stream) Abort() {
	s.framer.Abort()
	for _, f := range s.fields {
		if f.text != nil {
			f.text.Abort()
		}
		if f.tool != nil {
			f.tool.Abort()
		}
	}
	s.fields = nil
	s.closed = nil
	s.aliases = nil
	s.aliasBytes = 0
	s.ended = true
}
func (s *Stream) resolve(marker string) (string, bool) {
	v, ok := s.lookup(marker)
	if ok {
		if len(v) > s.budget {
			s.budgetExceeded = true
			return "", false
		}
		s.budget -= len(v)
	}
	return v, ok
}

// Push returns complete deliverable chunks. An empty result means DropChunk,
// not the SDK's empty-Body passthrough. On error the caller discards this
// transaction's output, aborts all pending state, and passes the current input.
func (s *Stream) Push(chunk []byte) ([]byte, []protocol.Reference, error) {
	if len(chunk) > s.limits.RequestStateBytes-s.Buffered() {
		return nil, nil, errors.New("stream_chunk_size_limit")
	}
	frames, err := s.framer.Push(chunk)
	if err != nil {
		return nil, nil, err
	}
	var out bytes.Buffer
	var refs []protocol.Reference
	jsonEvents := 0
	for _, frame := range frames {
		if frame.Close {
			if s.hasPending() {
				return nil, nil, errors.New("stream_closed_before_fields")
			}
			if !s.arrayStarted {
				out.WriteByte('[')
				s.arrayStarted = true
			}
			out.WriteByte(']')
			continue
		}
		if !frame.HasData {
			out.Write(frame.Render(frame.Data))
			continue
		}
		if bytes.Equal(bytes.TrimSpace(frame.Data), []byte("[DONE]")) {
			// Field-specific termination must already have happened. An outer
			// sentinel is not a substitute for missing tool/block completion.
			if s.hasPending() {
				return nil, nil, errors.New("stream_done_before_fields")
			}
			s.ended = true
			out.Write(frame.Render(frame.Data))
			continue
		}
		if s.ended {
			return nil, nil, errors.New("stream_event_after_end")
		}
		v, err := document.JSON(frame.Data)
		if err != nil {
			return nil, nil, errors.New("invalid_stream_event")
		}
		root, ok := v.(map[string]any)
		if !ok {
			return nil, nil, errors.New("stream_event_requires_object")
		}
		s.budget, s.budgetExceeded = s.limits.EventBytes, false
		extra, ids, err := s.event(root, frame.Event)
		if err != nil {
			return nil, nil, err
		}
		refs = append(refs, ids...)
		if s.budgetExceeded {
			return nil, nil, errors.New("restored_event_size_limit")
		}
		if s.format == "openai-response" || s.format == "codex" {
			if err := s.responseSequence(root, extra); err != nil {
				return nil, nil, err
			}
			if s.framer.Mode == "json" && len(extra) > 0 {
				if jsonEvents > 0 {
					return nil, nil, errors.New("multiple_bare_stream_events")
				}
				// Native Responses WebSocket chunks are bare JSON. CPA also
				// accepts SSE records here, allowing a tail delta before its
				// done event without concatenating bare JSON objects.
				s.responseSSE = true
			}
		}
		for _, event := range append(extra, root) {
			raw, err := json.Marshal(event)
			if err != nil || len(raw) > s.limits.EventBytes {
				return nil, nil, errors.New("restored_event_size_limit")
			}
			switch s.framer.Mode {
			case "sse":
				if event == nil {
					continue
				}
				if len(extra) > 0 && eventName(event) != eventName(root) {
					f := Frame{HasData: true}
					if name := eventName(event); name != "" {
						f.Lines = []string{"event: " + name}
					}
					out.Write(f.Render(raw))
				} else {
					out.Write(frame.Render(raw))
				}
			case "array":
				if !s.arrayStarted {
					out.WriteByte('[')
					s.arrayStarted = true
				}
				if s.arrayItem {
					out.WriteByte(',')
				}
				out.Write(raw)
				s.arrayItem = true
			case "json":
				if s.responseSSE {
					out.Write((Frame{HasData: true}).Render(raw))
					break
				}
				jsonEvents++
				if jsonEvents > 1 {
					return nil, nil, errors.New("multiple_bare_stream_events")
				}
				out.Write(raw)
			}
			if out.Len() > s.limits.RequestStateBytes {
				return nil, nil, errors.New("stream_output_size_limit")
			}
		}
		if s.Buffered() > s.limits.RequestStateBytes {
			return nil, nil, errors.New("stream_state_limit")
		}
	}
	if s.Buffered() > s.limits.RequestStateBytes {
		return nil, nil, errors.New("stream_state_limit")
	}
	return out.Bytes(), refs, nil
}

func (s *Stream) hasPending() bool {
	for _, f := range s.fields {
		if f.closed {
			continue
		}
		if f.text != nil && f.text.Buffered() > 0 {
			return true
		}
		if f.tool != nil && !f.tool.validated {
			return true
		}
	}
	return false
}
func (s *Stream) delta(key, group string, d delivery, m map[string]any, name string, tool bool) error {
	v, exists := m[name]
	if !exists || v == nil {
		return nil
	}
	fragment, ok := v.(string)
	if !ok {
		return errors.New("stream_delta_requires_string")
	}
	if s.closed[group] {
		return errors.New("stream_delta_after_close")
	}
	f := s.fields[key]
	if f == nil {
		if fragment == "" {
			return nil
		}
		if len(s.fields) >= MaxStreamFields {
			return errors.New("stream_field_limit")
		}
		f = &streamField{group: group, delivery: d}
		if tool {
			f.tool = NewToolJSON(s.resolve, s.limits.ToolJSONBytes)
		} else {
			f.text = NewText(s.resolve)
		}
		s.fields[key] = f
	}
	if f.closed || (f.tool != nil) != tool {
		return errors.New("stream_field_changed")
	}
	// Later events can supply IDs that were absent in the initial delta.
	if d.itemID != "" {
		f.delivery.itemID = strings.Clone(d.itemID)
	}
	if d.hasIndex {
		f.delivery.index, f.delivery.hasIndex = d.index, true
	}
	if tool {
		out, err := f.tool.Push(fragment, false)
		if err != nil {
			return err
		}
		m[name] = out
	} else {
		m[name] = f.text.Push(fragment, false)
	}
	return nil
}
func (s *Stream) finish(match func(string, *streamField) bool) ([]tail, error) {
	keys := make([]string, 0)
	for k, f := range s.fields {
		if !f.closed && match(k, f) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var tails []tail
	for _, k := range keys {
		f := s.fields[k]
		var text string
		if f.tool != nil {
			var err error
			text, err = f.tool.Push("", true)
			if err != nil {
				return nil, err
			}
		} else {
			text = f.text.Push("", true)
		}
		f.closed = true
		if text != "" {
			tails = append(tails, tail{f.delivery, text})
		}
	}
	return tails, nil
}
func (s *Stream) finishGroup(group string) ([]tail, error) {
	if len(s.closed) >= MaxStreamFields && !s.closed[group] {
		return nil, errors.New("stream_group_limit")
	}
	tails, err := s.finish(func(_ string, f *streamField) bool { return f.group == group })
	if err == nil {
		s.closed[group] = true
	}
	return tails, err
}
func (s *Stream) snapshot(component string, root map[string]any) ([]protocol.Reference, error) {
	doc, err := protocol.Snapshot(s.format, component, root, s.limits.ToolJSONBytes)
	if err != nil {
		return nil, err
	}
	for _, f := range doc.Fields {
		f.Set(token.Rewrite(f.Text, s.resolve))
	}
	if _, err = doc.Marshal(); err != nil {
		return nil, err
	}
	return doc.ResponseIDs, nil
}
func eventName(m map[string]any) string {
	if v, _ := m["event_type"].(string); v != "" {
		return v
	}
	v, _ := m["type"].(string)
	return v
}
func obj(v any) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("stream_object_required")
	}
	return m, nil
}
func arr(v any) ([]any, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, errors.New("stream_array_required")
	}
	return a, nil
}
func index(m map[string]any, name string, fallback int) (int, error) {
	v, ok := m[name]
	if !ok {
		return fallback, nil
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, errors.New("invalid_stream_index")
	}
	i, err := strconv.Atoi(string(n))
	if err != nil || i < 0 || i > 1<<30 {
		return 0, errors.New("invalid_stream_index")
	}
	return i, nil
}
func str(m map[string]any, name string) (string, error) {
	v, ok := m[name]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", errors.New("stream_string_required")
	}
	return s, nil
}
func (s *Stream) event(root map[string]any, name string) ([]map[string]any, []protocol.Reference, error) {
	if root["error"] != nil || name == "error" {
		return nil, nil, errors.New("upstream_stream_error")
	}
	switch s.format {
	case "openai":
		return s.chat(root)
	case "openai-response", "codex":
		return s.responses(root, name)
	case "claude":
		return s.claude(root, name)
	case "gemini", "antigravity":
		return s.gemini(root)
	case "interactions":
		return s.interactions(root, name)
	default:
		refs, err := s.snapshot("special", root)
		return nil, refs, err
	}
}

func tailEvents(tails []tail) []map[string]any {
	var out []map[string]any
	for _, t := range tails {
		d := t.delivery
		var event map[string]any
		switch d.family {
		case "claude":
			event = map[string]any{"type": "content_block_delta", "index": d.index, "delta": map[string]any{"type": d.deltaKind, d.field: t.text}}
		case "responses":
			event = map[string]any{"type": d.event, "delta": t.text}
			if d.itemID != "" {
				event["item_id"] = d.itemID
			}
			if d.hasIndex {
				event["output_index"] = d.index
			}
			if d.contentKey != "" {
				event[d.contentKey] = d.part
			}
		case "interactions":
			delta := map[string]any{"type": d.deltaKind, d.field: t.text}
			if d.deltaKind == "thought_summary" {
				delta = map[string]any{"type": d.deltaKind, "content": map[string]any{"type": "text", "text": t.text}}
			}
			event = map[string]any{"event_type": "step.delta", "index": d.index, "delta": delta}
		}
		if event != nil {
			out = append(out, event)
		}
	}
	return out
}
