package restore

import (
	"errors"
	"strconv"

	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
)

func (s *Stream) interactions(m map[string]any, name string) ([]map[string]any, []protocol.Reference, error) {
	if eventName(m) != "" {
		name = eventName(m)
	}
	i, err := index(m, "index", 0)
	if err != nil {
		return nil, nil, err
	}
	group := "i:" + strconv.Itoa(i)
	switch name {
	case "interaction.created", "interaction.in_progress":
		interaction, err := obj(m["interaction"])
		if err != nil {
			return nil, nil, err
		}
		refs, err := s.snapshot("interaction", interaction)
		return nil, refs, err
	case "step.start":
		step, err := obj(m["step"])
		if err != nil {
			return nil, nil, err
		}
		switch step["type"] {
		case "model_output", "user_input", "thought":
			if step["content"] == nil {
				return nil, nil, nil
			}
		}
		_, err = s.snapshot("interaction_step", step)
		return nil, nil, err
	case "step.delta":
		delta, err := obj(m["delta"])
		if err != nil {
			return nil, nil, err
		}
		kind, err := str(delta, "type")
		if err != nil {
			return nil, nil, err
		}
		field, tool := "text", false
		value := delta
		switch kind {
		case "text", "thinking", "reasoning":
			if kind != "text" && delta["text"] == nil {
				field = "thinking"
			}
		case "arguments_delta":
			field, tool = "arguments", true
		case "thought_summary":
			if v := delta["content"]; v != nil {
				value, err = obj(v)
				if err != nil {
					return nil, nil, err
				}
			}
		case "signature", "signature_delta":
			return nil, nil, nil
		case "function_call", "function_result", "image", "audio", "video", "code_execution_call", "code_execution_result":
			_, err = s.snapshot("interaction_step", delta)
			return nil, nil, err
		default:
			return nil, nil, errors.New("unsupported_interactions_stream_delta")
		}
		d := delivery{family: "interactions", index: i, field: field, deltaKind: kind}
		return nil, nil, s.delta(group+"/"+kind, group, d, value, field, tool)
	case "step.stop":
		tails, err := s.finishGroup(group)
		if err != nil {
			return nil, nil, err
		}
		if v := m["step"]; v != nil {
			step, err := obj(v)
			if err != nil {
				return nil, nil, err
			}
			if _, err := s.snapshot("interaction_step", step); err != nil {
				return nil, nil, err
			}
		}
		return tailEvents(tails), nil, nil
	case "interaction.completed", "finish":
		tails, err := s.finish(func(string, *streamField) bool { return true })
		if err != nil {
			return nil, nil, err
		}
		var refs []protocol.Reference
		if v := m["interaction"]; v != nil {
			interaction, err := obj(v)
			if err != nil {
				return nil, nil, err
			}
			refs, err = s.snapshot("interaction", interaction)
			if err != nil {
				return nil, nil, err
			}
		}
		s.ended = true
		return tailEvents(tails), refs, nil
	case "interaction.failed", "error":
		return nil, nil, errors.New("upstream_stream_error")
	case "ping":
		return nil, nil, nil
	default:
		return nil, nil, errors.New("unsupported_interactions_stream_event")
	}
}
