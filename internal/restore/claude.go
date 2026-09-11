package restore

import (
	"errors"
	"strconv"

	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
)

func (s *Stream) claude(m map[string]any, name string) ([]map[string]any, []protocol.Reference, error) {
	if eventName(m) != "" {
		name = eventName(m)
	}
	i, err := index(m, "index", 0)
	if err != nil {
		return nil, nil, err
	}
	group := "a:" + strconv.Itoa(i)
	switch name {
	case "message_start":
		message, err := obj(m["message"])
		if err != nil {
			return nil, nil, err
		}
		if message["content"] != nil {
			_, err = s.snapshot("claude_message", message)
		}
		return nil, nil, err
	case "content_block_start":
		block, err := obj(m["content_block"])
		if err != nil {
			return nil, nil, err
		}
		kind, err := str(block, "type")
		if err != nil {
			return nil, nil, err
		}
		if kind == "text" || kind == "thinking" {
			d := delivery{family: "claude", index: i, field: kind, deltaKind: kind + "_delta"}
			return nil, nil, s.delta(group+"/"+kind, group, d, block, kind, false)
		}
		_, err = s.snapshot("claude_block", block)
		return nil, nil, err
	case "content_block_delta":
		delta, err := obj(m["delta"])
		if err != nil {
			return nil, nil, err
		}
		kind, err := str(delta, "type")
		if err != nil {
			return nil, nil, err
		}
		field, tool := "", false
		switch kind {
		case "text_delta":
			field = "text"
		case "thinking_delta":
			field = "thinking"
		case "input_json_delta":
			field, tool = "partial_json", true
		case "signature_delta", "citations_delta":
			return nil, nil, nil
		default:
			return nil, nil, errors.New("unsupported_claude_stream_delta")
		}
		d := delivery{family: "claude", index: i, field: field, deltaKind: kind}
		return nil, nil, s.delta(group+"/"+field, group, d, delta, field, tool)
	case "content_block_stop":
		tails, err := s.finishGroup(group)
		return tailEvents(tails), nil, err
	case "message_delta":
		delta, err := obj(m["delta"])
		if err != nil {
			return nil, nil, err
		}
		stop, err := str(delta, "stop_reason")
		if err != nil {
			return nil, nil, err
		}
		if stop != "" {
			tails, err := s.finish(func(string, *streamField) bool { return true })
			return tailEvents(tails), nil, err
		}
	case "message_stop":
		tails, err := s.finish(func(string, *streamField) bool { return true })
		s.ended = err == nil
		return tailEvents(tails), nil, err
	case "ping":
	case "error":
		return nil, nil, errors.New("upstream_stream_error")
	default:
		return nil, nil, errors.New("unsupported_claude_stream_event")
	}
	return nil, nil, nil
}
