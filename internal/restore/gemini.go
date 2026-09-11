package restore

import (
	"errors"
	"strconv"

	"github.com/kkkykin/cpa-vibeguard/internal/protocol"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

func (s *Stream) gemini(root map[string]any) ([]map[string]any, []protocol.Reference, error) {
	m := root
	if s.format == "antigravity" && root["response"] != nil {
		var err error
		m, err = obj(root["response"])
		if err != nil {
			return nil, nil, err
		}
	}
	if v := m["candidates"]; v != nil {
		a, err := arr(v)
		if err != nil {
			return nil, nil, err
		}
		for pos, v := range a {
			c, err := obj(v)
			if err != nil {
				return nil, nil, err
			}
			i, err := index(c, "index", pos)
			if err != nil {
				return nil, nil, err
			}
			group := "g:" + strconv.Itoa(i)
			if v := c["content"]; v != nil {
				content, err := obj(v)
				if err != nil {
					return nil, nil, err
				}
				parts, err := arr(content["parts"])
				if err != nil {
					return nil, nil, err
				}
				for j, v := range parts {
					part, err := obj(v)
					if err != nil {
						return nil, nil, err
					}
					if _, ok := part["text"]; ok {
						thought := false
						if v, ok := part["thought"]; ok {
							var valid bool
							thought, valid = v.(bool)
							if !valid {
								return nil, nil, errors.New("invalid_thought_flag")
							}
						}
						d := delivery{family: "gemini", field: "text", index: i, part: j, thought: thought}
						key := group + "/" + strconv.Itoa(j) + "/" + strconv.FormatBool(thought)
						// Validate the known part shape without restoring text as a snapshot.
						if _, err := protocol.Snapshot(s.format, "gemini_part", part, s.limits.ToolJSONBytes); err != nil {
							return nil, nil, err
						}
						if err := s.delta(key, group, d, part, "text", false); err != nil {
							return nil, nil, err
						}
					} else if _, err := s.snapshot("gemini_part", part); err != nil {
						return nil, nil, err
					}
				}
			}
			finish, err := str(c, "finishReason")
			if err != nil {
				return nil, nil, err
			}
			if finish != "" {
				tails, err := s.finishGroup(group)
				if err != nil {
					return nil, nil, err
				}
				for _, t := range tails {
					if err := addGeminiTail(c, t); err != nil {
						return nil, nil, err
					}
				}
			}
		}
	}
	if v := m["promptFeedback"]; v != nil {
		feedback, err := obj(v)
		if err != nil {
			return nil, nil, err
		}
		if text, err := str(feedback, "blockReasonMessage"); err != nil {
			return nil, nil, err
		} else if _, ok := feedback["blockReasonMessage"]; ok {
			feedback["blockReasonMessage"] = token.Rewrite(text, s.resolve)
		}
	}
	return nil, nil, nil
}
func addGeminiTail(c map[string]any, t tail) error {
	content, err := child(c, "content")
	if err != nil {
		return err
	}
	parts, _ := content["parts"].([]any)
	d := t.delivery
	if d.part < len(parts) {
		part, err := obj(parts[d.part])
		if err != nil {
			return err
		}
		thought, _ := part["thought"].(bool)
		if _, ok := part["text"]; ok && thought == d.thought {
			return addString(part, "text", t.text)
		}
	}
	part := map[string]any{"text": t.text}
	if d.thought {
		part["thought"] = true
	}
	content["parts"] = append(parts, part)
	if content["role"] == nil {
		content["role"] = "model"
	}
	return nil
}
