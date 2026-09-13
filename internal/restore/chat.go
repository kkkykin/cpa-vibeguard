package restore

import (
	"errors"
	"strconv"
	"strings"

	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
)

type chatToolKey struct{ choice, tool int }

// A tool's type is commonly sent only in its first delta, which can be empty
// or metadata-only. Keep that type independently of buffered text fields.
func (s *Stream) chatToolType(call map[string]any, key chatToolKey) (string, error) {
	kind, err := str(call, "type")
	if err != nil {
		return "", err
	}
	if kind != "" && kind != "function" && kind != "custom" {
		return "", errors.New("unsupported_chat_stream_tool")
	}
	wrapper := ""
	for _, name := range []string{"function", "custom"} {
		if call[name] != nil {
			if wrapper != "" || (kind != "" && kind != name) {
				return "", errors.New("mixed_chat_stream_tool")
			}
			wrapper = name
		}
	}
	if wrapper != "" {
		kind = wrapper
	}
	previous := s.chatTools[key]
	if kind == "" {
		return previous, nil
	}
	if previous != "" && previous != kind {
		return "", errors.New("chat_stream_tool_type_changed")
	}
	if previous == "" {
		if len(s.chatTools) >= MaxStreamFields {
			return "", errors.New("stream_tool_limit")
		}
		s.chatTools[key] = kind
	}
	return kind, nil
}

func (s *Stream) chat(root map[string]any) ([]map[string]any, []protocol.Reference, error) {
	choices, err := arr(root["choices"])
	if err != nil {
		return nil, nil, err
	}
	for pos, v := range choices {
		c, err := obj(v)
		if err != nil {
			return nil, nil, err
		}
		i, err := index(c, "index", pos)
		if err != nil {
			return nil, nil, err
		}
		group := "c:" + strconv.Itoa(i)
		if v := c["delta"]; v != nil {
			delta, err := obj(v)
			if err != nil {
				return nil, nil, err
			}
			if err = s.chatDelta(delta, group, i); err != nil {
				return nil, nil, err
			}
		}
		if v := c["message"]; v != nil {
			m, err := obj(v)
			if err != nil {
				return nil, nil, err
			}
			if _, err = s.snapshot("chat_message", m); err != nil {
				return nil, nil, err
			}
		}
		if err = s.delta(group+"/legacy", group, delivery{family: "chat", field: "legacy", index: i}, c, "text", false); err != nil {
			return nil, nil, err
		}
		finish, err := str(c, "finish_reason")
		if err != nil {
			return nil, nil, err
		}
		if finish != "" {
			tails, err := s.finishGroup(group)
			if err != nil {
				return nil, nil, err
			}
			for _, t := range tails {
				if err := addChatTail(c, t); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	return nil, nil, nil
}
func (s *Stream) chatDelta(m map[string]any, group string, i int) error {
	for _, k := range []string{"content", "refusal", "reasoning", "reasoning_content"} {
		if a, ok := m[k].([]any); ok && k == "content" {
			for j, v := range a {
				part, err := obj(v)
				if err != nil {
					return err
				}
				kind, err := str(part, "type")
				if err != nil {
					return err
				}
				switch kind {
				case "text", "output_text":
					d := delivery{family: "chat", field: "content.text", index: i, part: j}
					if err := s.delta(group+"/part:"+strconv.Itoa(j), group, d, part, "text", false); err != nil {
						return err
					}
				case "image_url", "audio", "input_audio":
				default:
					return errors.New("unsupported_chat_stream_part")
				}
			}
		} else if err := s.delta(group+"/"+k, group, delivery{family: "chat", field: k, index: i}, m, k, false); err != nil {
			return err
		}
	}
	if v := m["audio"]; v != nil {
		audio, err := obj(v)
		if err != nil {
			return err
		}
		if err := s.delta(group+"/audio", group, delivery{family: "chat", field: "audio.transcript", index: i}, audio, "transcript", false); err != nil {
			return err
		}
	}
	if v := m["function_call"]; v != nil {
		call, err := obj(v)
		if err != nil {
			return err
		}
		if err := s.delta(group+"/function", group, delivery{family: "chat", field: "function_call", index: i, tool: -1}, call, "arguments", true); err != nil {
			return err
		}
	}
	if v := m["tool_calls"]; v != nil {
		calls, err := arr(v)
		if err != nil {
			return err
		}
		for pos, v := range calls {
			call, err := obj(v)
			if err != nil {
				return err
			}
			j, err := index(call, "index", pos)
			if err != nil {
				return err
			}
			if s.closed[group] {
				return errors.New("stream_delta_after_close")
			}
			field, err := s.chatToolType(call, chatToolKey{choice: i, tool: j})
			if err != nil {
				return err
			}
			if field == "" {
				continue
			}
			key, tool := "arguments", true
			if field == "custom" {
				key, tool = "input", false
			}
			if v := call[field]; v != nil {
				f, err := obj(v)
				if err != nil {
					return err
				}
				d := delivery{family: "chat", field: field + "." + key, index: i, tool: j}
				if err := s.delta(group+"/tool:"+strconv.Itoa(j)+"/"+field, group, d, f, key, tool); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func addString(m map[string]any, k, tail string) error {
	v, err := str(m, k)
	if err != nil {
		return err
	}
	m[k] = v + tail
	return nil
}
func child(m map[string]any, k string) (map[string]any, error) {
	if v := m[k]; v != nil {
		return obj(v)
	}
	n := make(map[string]any)
	m[k] = n
	return n, nil
}
func addChatTail(choice map[string]any, t tail) error {
	d := t.delivery
	if d.field == "legacy" {
		return addString(choice, "text", t.text)
	}
	m, err := child(choice, "delta")
	if err != nil {
		return err
	}
	switch d.field {
	case "content.text":
		a, _ := m["content"].([]any)
		for len(a) <= d.part {
			a = append(a, map[string]any{"type": "text", "text": ""})
		}
		p, err := obj(a[d.part])
		if err != nil {
			return err
		}
		m["content"] = a
		return addString(p, "text", t.text)
	case "audio.transcript":
		a, err := child(m, "audio")
		if err != nil {
			return err
		}
		return addString(a, "transcript", t.text)
	case "function_call":
		f, err := child(m, "function_call")
		if err != nil {
			return err
		}
		return addString(f, "arguments", t.text)
	case "function.arguments", "custom.input":
		a, _ := m["tool_calls"].([]any)
		var call map[string]any
		for pos, v := range a {
			c, err := obj(v)
			if err != nil {
				return err
			}
			i, err := index(c, "index", pos)
			if err != nil {
				return err
			}
			if i == d.tool {
				call = c
				break
			}
		}
		if call == nil {
			call = map[string]any{"index": d.tool}
			a = append(a, call)
			m["tool_calls"] = a
		}
		field, key, _ := strings.Cut(d.field, ".")
		f, err := child(call, field)
		if err != nil {
			return err
		}
		return addString(f, key, t.text)
	default:
		return addString(m, d.field, t.text)
	}
}
