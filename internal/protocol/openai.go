package protocol

import (
	"errors"
	"fmt"
)

func (d *Document) chatRequest() error {
	m := d.Body
	if err := rejectKeys(m, "input", "contents"); err != nil {
		return err
	}
	if v, exists := m["prompt"]; exists {
		if _, mixed := m["messages"]; mixed {
			return errors.New("mixed_protocol_structure")
		}
		switch x := v.(type) {
		case string:
			if err := d.str(m, "prompt", "", true); err != nil {
				return err
			}
		case []any:
			for i, v := range x {
				s, ok := v.(string)
				if !ok {
					return errors.New("legacy_prompt_requires_text")
				}
				d.Fields = append(d.Fields, Field{Path: pathIndex("/prompt", i), Text: s, set: func(s string) { x[i] = s }})
			}
		default:
			return errors.New("legacy_prompt_requires_text")
		}
	} else {
		messages, err := array(m["messages"], "messages")
		if err != nil || len(messages) == 0 {
			return errors.New("messages_required")
		}
		for i, v := range messages {
			msg, err := object(v, "messages")
			if err != nil {
				return err
			}
			if err := d.chatMessage(msg, pathIndex("/messages", i), true); err != nil {
				return err
			}
		}
	}
	if v, exists := m["prediction"]; exists && v != nil {
		if err := d.chatPrediction(v); err != nil {
			return err
		}
	}
	return d.openaiTools(m)
}

func (d *Document) chatPrediction(v any) error {
	m, err := object(v, "/prediction")
	if err != nil {
		return err
	}
	if kind(m) != "content" {
		return errors.New("unsupported_prediction_type")
	}
	if _, ok := m["content"].(string); ok {
		return d.str(m, "content", "/prediction", true)
	}
	parts, err := array(m["content"], "/prediction/content")
	if err != nil {
		return err
	}
	for i, v := range parts {
		path := pathIndex("/prediction/content", i)
		part, err := object(v, path)
		if err != nil {
			return err
		}
		if kind(part) != "text" {
			return errors.New("unsupported_prediction_content_node")
		}
		if err := d.str(part, "text", path, true); err != nil {
			return err
		}
	}
	return nil
}

func (d *Document) chatMessage(m map[string]any, path string, request bool) error {
	if request {
		role, ok := m["role"].(string)
		if !ok {
			return errors.New("message_role_required")
		}
		switch role {
		case "system", "developer", "user", "assistant", "tool", "function":
		default:
			return errors.New("unsupported_message_role")
		}
	}
	if v, ok := m["content"]; ok && v != nil {
		if err := d.textContent(m, "content", path, "openai"); err != nil {
			return err
		}
	} else if request && m["tool_calls"] == nil && m["function_call"] == nil && m["role"] != "assistant" {
		return errors.New("message_content_required")
	}
	for _, key := range []string{"refusal", "reasoning_content", "reasoning"} {
		if err := d.str(m, key, path, false); err != nil {
			return err
		}
	}
	if v, ok := m["audio"]; ok && v != nil {
		audio, err := object(v, path+"/audio")
		if err != nil {
			return err
		}
		if err := d.str(audio, "transcript", path+"/audio", false); err != nil {
			return err
		}
	}
	if v, ok := m["tool_calls"]; ok && v != nil {
		a, err := array(v, path+"/tool_calls")
		if err != nil {
			return err
		}
		for i, v := range a {
			call, err := object(v, path+"/tool_calls")
			if err != nil {
				return err
			}
			p := pathIndex(path+"/tool_calls", i)
			switch kind(call) {
			case "function", "":
				f, err := object(call["function"], p+"/function")
				if err != nil {
					return err
				}
				if err := d.toolJSON(f, "arguments", p+"/function"); err != nil {
					return err
				}
			case "custom":
				c, err := object(call["custom"], p+"/custom")
				if err != nil {
					return err
				}
				if err := d.str(c, "input", p+"/custom", true); err != nil {
					return err
				}
			default:
				return errors.New("unsupported_tool_call_type")
			}
		}
	}
	if v, ok := m["function_call"]; ok && v != nil {
		f, err := object(v, path+"/function_call")
		if err != nil {
			return err
		}
		if err := d.toolJSON(f, "arguments", path+"/function_call"); err != nil {
			return err
		}
	}
	return nil
}
func (d *Document) textContent(m map[string]any, key, path, family string) error {
	v, exists := m[key]
	if !exists {
		return fmt.Errorf("content_required: %s/%s", path, key)
	}
	if _, ok := v.(string); ok {
		return d.str(m, key, path, true)
	}
	a, err := array(v, path+"/"+key)
	if err != nil {
		return err
	}
	for i, v := range a {
		part, err := object(v, path+"/"+key)
		if err != nil {
			return err
		}
		if err := d.textPart(part, pathIndex(path+"/"+key, i), family); err != nil {
			return err
		}
	}
	return nil
}
func (d *Document) textPart(m map[string]any, path, family string) error {
	switch kind(m) {
	case "text", "input_text", "output_text", "summary_text", "reasoning_text":
		return d.str(m, "text", path, true)
	case "refusal":
		return d.str(m, "refusal", path, true)
	case "image_url", "input_image", "image", "input_audio", "audio", "video", "video_url", "input_video", "input_file", "file":
		return nil
	default:
		return fmt.Errorf("unsupported_%s_content_node", family)
	}
}
func (d *Document) openaiTools(m map[string]any) error {
	for _, key := range []string{"tools", "functions"} {
		if v, exists := m[key]; exists {
			a, err := array(v, key)
			if err != nil {
				return err
			}
			for i, v := range a {
				x, err := object(v, key)
				if err != nil {
					return err
				}
				path := pathIndex("/"+key, i)
				// Chat wraps function/custom definitions; Responses also permits
				// flat definitions. Select exactly one wrapper before visiting text.
				wrapper := ""
				for _, name := range []string{"function", "custom"} {
					if _, exists := x[name]; exists {
						if wrapper != "" {
							return errors.New("mixed_tool_definition")
						}
						wrapper = name
					}
				}
				if wrapper != "" {
					if k := kind(x); k != "" && k != wrapper {
						return errors.New("mixed_tool_definition")
					}
					path += "/" + wrapper
					x, err = object(x[wrapper], path)
					if err != nil {
						return err
					}
				}
				if kind(x) == "mcp" {
					if err := d.str(x, "server_description", path, false); err != nil {
						return err
					}
				}
				if err := d.toolDefinition(x, path); err != nil {
					return err
				}
			}
		}
	}
	if v, exists := m["response_format"]; exists {
		f, err := object(v, "response_format")
		if err != nil {
			return err
		}
		if v, ok := f["json_schema"]; ok {
			j, err := object(v, "response_format/json_schema")
			if err != nil {
				return err
			}
			if err := d.str(j, "description", "/response_format/json_schema", false); err != nil {
				return err
			}
			if v, ok := j["schema"]; ok {
				return d.schema(v, "/response_format/json_schema/schema")
			}
		}
	}
	return nil
}
func (d *Document) chatResponse() error {
	m := d.Body
	choices, err := array(m["choices"], "choices")
	if err != nil {
		return err
	}
	for i, v := range choices {
		c, err := object(v, "choices")
		if err != nil {
			return err
		}
		p := pathIndex("/choices", i)
		if v, ok := c["message"]; ok {
			msg, err := object(v, p+"/message")
			if err != nil {
				return err
			}
			if err := d.chatMessage(msg, p+"/message", false); err != nil {
				return err
			}
		}
		if err := d.str(c, "text", p, false); err != nil {
			return err
		}
	}
	return nil
}

func (d *Document) responsesRequest() error {
	m := d.Body
	if err := rejectKeys(m, "messages", "contents"); err != nil {
		return err
	}
	if err := d.str(m, "instructions", "", false); err != nil {
		return err
	}
	input, exists := m["input"]
	if !exists {
		return errors.New("responses_input_required")
	}
	if _, ok := input.(string); ok {
		if err := d.str(m, "input", "", true); err != nil {
			return err
		}
	} else {
		a, err := array(input, "input")
		if err != nil {
			return err
		}
		for i, v := range a {
			x, err := object(v, "input")
			if err != nil {
				return err
			}
			if err := d.responseItem(x, pathIndex("/input", i)); err != nil {
				return err
			}
		}
	}
	if err := d.reference(m, "previous_response_id", "response", false); err != nil {
		return err
	}
	if err := d.openaiTools(m); err != nil {
		return err
	}
	return d.responsesTextFormat(m, "")
}

func (d *Document) responsesTextFormat(m map[string]any, path string) error {
	v, exists := m["text"]
	if !exists {
		return nil
	}
	text, err := object(v, path+"/text")
	if err != nil {
		return err
	}
	v, exists = text["format"]
	if !exists {
		return nil
	}
	format, err := object(v, path+"/text/format")
	if err != nil {
		return err
	}
	if err := d.str(format, "description", path+"/text/format", false); err != nil {
		return err
	}
	if schema, exists := format["schema"]; exists {
		return d.schema(schema, path+"/text/format/schema")
	}
	return nil
}

func (d *Document) responsesResponse(m map[string]any, path string) error {
	if v, ok := m["response"]; ok {
		x, err := object(v, path+"/response")
		if err != nil {
			return err
		}
		return d.responsesResponse(x, path+"/response")
	}
	if err := d.reference(m, "id", "response", true); err != nil {
		return err
	}
	if err := d.str(m, "instructions", path, false); err != nil {
		return err
	}
	// Responses can echo the redacted request's tool definitions. They are
	// complete fields in both ordinary responses and terminal stream snapshots.
	if err := d.openaiTools(m); err != nil {
		return err
	}
	if err := d.responsesTextFormat(m, path); err != nil {
		return err
	}
	if err := d.str(m, "output_text", path, false); err != nil {
		return err
	}
	if v, ok := m["output"]; ok {
		a, err := array(v, path+"/output")
		if err != nil {
			return err
		}
		for i, v := range a {
			x, err := object(v, path+"/output")
			if err != nil {
				return err
			}
			if err := d.responseItem(x, pathIndex(path+"/output", i)); err != nil {
				return err
			}
		}
	} else if _, exists := m["output_text"]; !exists {
		return errors.New("responses_output_required")
	}
	return nil
}
func (d *Document) responseItem(m map[string]any, path string) error {
	switch kind(m) {
	case "message", "":
		role, ok := m["role"].(string)
		if !ok {
			return errors.New("response_message_role_required")
		}
		switch role {
		case "user", "assistant", "system", "developer":
		default:
			return errors.New("unsupported_message_role")
		}
		return d.textContent(m, "content", path, "responses")
	case "function_call":
		return d.toolJSON(m, "arguments", path)
	case "function_call_output", "custom_tool_call_output":
		return d.toolResult(m, "output", path)
	case "custom_tool_call":
		return d.str(m, "input", path, true)
	case "reasoning":
		for _, key := range []string{"summary", "content"} {
			if _, exists := m[key]; exists {
				if err := d.textContent(m, key, path, "responses"); err != nil {
					return err
				}
			}
		}
		return nil
	case "compaction", "item_reference":
		return nil
	case "web_search_call":
		if v, ok := m["action"]; ok {
			a, err := object(v, path+"/action")
			if err != nil {
				return err
			}
			if err := d.str(a, "query", path+"/action", false); err != nil {
				return err
			}
			if v, ok := a["queries"]; ok {
				return d.strings(v, func(v any) { a["queries"] = v }, path+"/action/queries")
			}
		}
		return nil
	case "file_search_call":
		if v, ok := m["queries"]; ok {
			if err := d.strings(v, func(v any) { m["queries"] = v }, path+"/queries"); err != nil {
				return err
			}
		}
		if v, ok := m["results"]; ok {
			a, err := array(v, path+"/results")
			if err != nil {
				return err
			}
			for i, v := range a {
				x, err := object(v, path+"/results")
				if err != nil {
					return err
				}
				if err := d.str(x, "text", pathIndex(path+"/results", i), false); err != nil {
					return err
				}
			}
		}
		return nil
	case "code_interpreter_call":
		if err := d.str(m, "code", path, false); err != nil {
			return err
		}
		if v, ok := m["outputs"]; ok {
			a, err := array(v, path+"/outputs")
			if err != nil {
				return err
			}
			for i, v := range a {
				x, err := object(v, path+"/outputs")
				if err != nil {
					return err
				}
				switch kind(x) {
				case "logs":
					if err := d.str(x, "logs", pathIndex(path+"/outputs", i), true); err != nil {
						return err
					}
				case "image":
				default:
					return errors.New("unsupported_code_output")
				}
			}
		}
		return nil
	case "image_generation_call":
		for _, k := range []string{"prompt", "revised_prompt"} {
			if err := d.str(m, k, path, false); err != nil {
				return err
			}
		}
		return nil
	case "computer_call":
		if v, ok := m["action"]; ok {
			x, err := object(v, path+"/action")
			if err != nil {
				return err
			}
			return d.str(x, "text", path+"/action", false)
		}
		return nil
	case "computer_call_output":
		return nil
	case "local_shell_call", "shell_call":
		if v, ok := m["action"]; ok {
			x, err := object(v, path+"/action")
			if err != nil {
				return err
			}
			for _, key := range []string{"command", "commands", "env"} {
				if v, ok := x[key]; ok {
					if err := d.strings(v, func(v any) { x[key] = v }, path+"/action/"+key); err != nil {
						return err
					}
				}
			}
		}
		return nil
	case "local_shell_call_output", "shell_call_output", "apply_patch_call_output":
		return d.toolResult(m, "output", path)
	case "apply_patch_call":
		if v, ok := m["operation"]; ok {
			x, err := object(v, path+"/operation")
			if err != nil {
				return err
			}
			return d.str(x, "diff", path+"/operation", false)
		}
		return nil
	case "mcp_call", "mcp_approval_request":
		if err := d.toolJSON(m, "arguments", path); err != nil {
			return err
		}
		if _, ok := m["output"]; ok {
			return d.toolResult(m, "output", path)
		}
		return nil
	case "mcp_approval_response":
		return d.str(m, "reason", path, false)
	case "mcp_list_tools":
		if v, ok := m["tools"]; ok {
			a, err := array(v, path+"/tools")
			if err != nil {
				return err
			}
			for i, v := range a {
				x, err := object(v, path+"/tools")
				if err != nil {
					return err
				}
				if err := d.toolDefinition(x, pathIndex(path+"/tools", i)); err != nil {
					return err
				}
			}
		}
		return nil
	default:
		return errors.New("unsupported_responses_item")
	}
}
