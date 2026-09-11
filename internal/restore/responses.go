package restore

import (
	"errors"
	"strconv"
	"strings"

	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

func (s *Stream) responseGroup(m map[string]any) (string, delivery, error) {
	d := delivery{family: "responses"}
	var err error
	d.index, err = index(m, "output_index", 0)
	if err != nil {
		return "", d, err
	}
	_, d.hasIndex = m["output_index"]
	d.itemID, err = str(m, "item_id")
	if err != nil {
		return "", d, err
	}
	if item, ok := m["item"].(map[string]any); ok {
		id, err := str(item, "id")
		if err != nil {
			return "", d, err
		}
		if id != "" {
			if d.itemID != "" && d.itemID != id {
				return "", d, errors.New("stream_item_id_changed")
			}
			d.itemID = id
		}
	}
	if len(d.itemID) > 1024 {
		return "", d, errors.New("stream_item_id_limit")
	}
	var keys []string
	if d.hasIndex {
		keys = append(keys, "index:"+strconv.Itoa(d.index))
	}
	if d.itemID != "" {
		keys = append(keys, "id:"+d.itemID)
	}
	if len(keys) == 0 {
		return "", d, errors.New("stream_output_identity_missing")
	}
	group := ""
	for _, k := range keys {
		if found := s.aliases[k]; found != "" {
			if group != "" && group != found {
				return "", d, errors.New("stream_output_identity_conflict")
			}
			group = found
		}
	}
	if group == "" {
		group = "r:" + keys[0]
	}
	for _, k := range keys {
		if s.aliases[k] == "" {
			if len(s.aliases) >= MaxStreamFields*2 {
				return "", d, errors.New("stream_identity_limit")
			}
			s.aliases[strings.Clone(k)] = group
			s.aliasBytes += 64 + len(k) + len(group)
		}
	}
	return group, d, nil
}
func responseDeltaKind(name string) (field, contentKey string, tool bool, ok bool) {
	switch name {
	case "response.output_text.delta", "response.reasoning_text.delta", "response.audio_transcript.delta", "response.output_audio_transcript.delta":
		return "text", "content_index", false, true
	case "response.reasoning_summary_text.delta":
		return "text", "summary_index", false, true
	case "response.refusal.delta":
		return "refusal", "content_index", false, true
	case "response.function_call_arguments.delta", "response.mcp_call_arguments.delta":
		return "arguments", "", true, true
	case "response.custom_tool_call_input.delta":
		return "input", "", false, true
	case "response.code_interpreter_call_code.delta":
		return "code", "", false, true
	}
	return "", "", false, false
}
func (s *Stream) responseSnapshot(m map[string]any) ([]protocol.Reference, error) {
	// Created/in-progress objects may omit output. This is a known partial
	// envelope; any output that is present still uses the complete codecs.
	_, exists := m["output"]
	_, textExists := m["output_text"]
	if !exists && !textExists {
		m["output"] = []any{}
		defer delete(m, "output")
	}
	return s.snapshot("response", m)
}
func (s *Stream) responseItem(m map[string]any, added bool) error {
	if added && m["type"] == "function_call" {
		args, err := str(m, "arguments")
		if err != nil {
			return err
		}
		if args == "" {
			return nil
		}
	}
	_, err := s.snapshot("response_item", m)
	return err
}
func (s *Stream) responses(m map[string]any, name string) ([]map[string]any, []protocol.Reference, error) {
	if eventName(m) != "" {
		name = eventName(m)
	}
	base := name
	if strings.HasSuffix(name, ".done") {
		base = strings.TrimSuffix(name, ".done") + ".delta"
	}
	if field, contentKey, tool, ok := responseDeltaKind(base); ok {
		group, d, err := s.responseGroup(m)
		if err != nil {
			return nil, nil, err
		}
		d.event, d.field, d.contentKey = base, field, contentKey
		if contentKey != "" {
			d.part, err = index(m, contentKey, 0)
			if err != nil {
				return nil, nil, err
			}
		}
		key := group + "/" + base + "/" + strconv.Itoa(d.part)
		if name == base {
			return nil, nil, s.delta(key, group, d, m, "delta", tool)
		}
		tails, err := s.finish(func(k string, _ *streamField) bool { return k == key })
		if err != nil {
			return nil, nil, err
		}
		value, err := str(m, field)
		if err != nil {
			return nil, nil, err
		}
		if tool {
			codec := NewToolJSON(s.resolve, s.limits.ToolJSONBytes)
			value, err = codec.Push(value, true)
			if err != nil {
				return nil, nil, err
			}
		} else {
			value = token.Rewrite(value, s.resolve)
		}
		m[field] = value
		return tailEvents(tails), nil, nil
	}
	switch name {
	case "response.created", "response.in_progress", "response.queued":
		response, err := obj(m["response"])
		if err != nil {
			return nil, nil, err
		}
		refs, err := s.responseSnapshot(response)
		return nil, refs, err
	case "response.output_item.added", "response.output_item.done":
		group, _, err := s.responseGroup(m)
		if err != nil {
			return nil, nil, err
		}
		var tails []tail
		if name == "response.output_item.done" {
			tails, err = s.finishGroup(group)
			if err != nil {
				return nil, nil, err
			}
		}
		item, err := obj(m["item"])
		if err != nil {
			return nil, nil, err
		}
		err = s.responseItem(item, name == "response.output_item.added")
		return tailEvents(tails), nil, err
	case "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		group, _, err := s.responseGroup(m)
		if err != nil {
			return nil, nil, err
		}
		contentKey := "content_index"
		if strings.Contains(name, "summary") {
			contentKey = "summary_index"
		}
		i, err := index(m, contentKey, 0)
		if err != nil {
			return nil, nil, err
		}
		var tails []tail
		if strings.HasSuffix(name, ".done") {
			tails, err = s.finish(func(_ string, f *streamField) bool {
				return f.group == group && f.delivery.part == i && f.delivery.contentKey == contentKey
			})
			if err != nil {
				return nil, nil, err
			}
		}
		part, err := obj(m["part"])
		if err != nil {
			return nil, nil, err
		}
		_, err = s.snapshot("response_part", part)
		return tailEvents(tails), nil, err
	case "response.completed", "response.incomplete":
		tails, err := s.finish(func(string, *streamField) bool { return true })
		if err != nil {
			return nil, nil, err
		}
		response, err := obj(m["response"])
		if err != nil {
			return nil, nil, err
		}
		refs, err := s.responseSnapshot(response)
		s.ended = err == nil
		return tailEvents(tails), refs, err
	case "response.failed", "error":
		return nil, nil, errors.New("upstream_stream_error")
	case "response.audio.delta", "response.audio.done", "response.output_audio.delta", "response.output_audio.done",
		"response.image_generation_call.partial_image", "response.output_text.annotation.added":
		return nil, nil, nil
	default:
		if strings.HasSuffix(name, ".delta") || strings.HasSuffix(name, ".done") {
			return nil, nil, errors.New("unsupported_responses_stream_field")
		}
		if !strings.HasPrefix(name, "response.") {
			return nil, nil, errors.New("unsupported_responses_stream_event")
		}
		// Known protocol progress events contain routing/status information.
		return nil, nil, nil
	}
}
