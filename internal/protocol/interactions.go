package protocol

import "errors"

func (d *Document) interactionsRequest() error {
	m := d.Body
	if err := rejectKeys(m, "messages", "contents", "instructions"); err != nil {
		return err
	}
	if err := d.str(m, "system_instruction", "", false); err != nil {
		return err
	}
	v, exists := m["input"]
	if !exists {
		return errors.New("interactions_input_required")
	}
	if _, ok := v.(string); ok {
		if err := d.str(m, "input", "", true); err != nil {
			return err
		}
	} else {
		a, err := array(v, "input")
		if err != nil {
			return err
		}
		for i, v := range a {
			x, err := object(v, "input")
			if err != nil {
				return err
			}
			if err := d.interactionStep(x, pathIndex("/input", i)); err != nil {
				return err
			}
		}
	}
	if err := d.reference(m, "previous_interaction_id", "interaction", false); err != nil {
		return err
	}
	if v, exists := m["tools"]; exists {
		a, err := array(v, "tools")
		if err != nil {
			return err
		}
		for i, v := range a {
			x, err := object(v, "tools")
			if err != nil {
				return err
			}
			if err := d.toolDefinition(x, pathIndex("/tools", i)); err != nil {
				return err
			}
		}
	}
	return nil
}
func (d *Document) interactionStep(m map[string]any, path string) error {
	switch kind(m) {
	case "user_input", "model_output", "thought":
		return d.interactionContent(m, "content", path)
	case "":
		if role, ok := m["role"].(string); ok && (role == "user" || role == "model" || role == "assistant") {
			return d.interactionContent(m, "content", path)
		}
		if _, ok := m["text"]; ok {
			return d.str(m, "text", path, true)
		}
		return errors.New("unsupported_interaction_step")
	case "function_call":
		if _, ok := m["arguments"].(string); ok {
			return d.toolJSON(m, "arguments", path)
		}
		return d.toolObject(m, "arguments", path, true)
	case "function_result":
		return d.toolResult(m, "result", path)
	case "text", "image", "audio", "video", "document", "thought_summary", "thinking", "reasoning", "code_execution_call", "code_execution_result":
		return d.interactionPart(m, path)
	default:
		return errors.New("unsupported_interaction_step")
	}
}
func (d *Document) interactionContent(m map[string]any, key, path string) error {
	v := m[key]
	if _, ok := v.(string); ok {
		return d.str(m, key, path, true)
	}
	if x, ok := v.(map[string]any); ok {
		return d.interactionPart(x, path+"/"+key)
	}
	a, err := array(v, path+"/"+key)
	if err != nil {
		return err
	}
	for i, v := range a {
		x, err := object(v, path+"/"+key)
		if err != nil {
			return err
		}
		if err := d.interactionPart(x, pathIndex(path+"/"+key, i)); err != nil {
			return err
		}
	}
	return nil
}
func (d *Document) interactionPart(m map[string]any, path string) error {
	switch kind(m) {
	case "", "text":
		return d.str(m, "text", path, true)
	case "thought_summary":
		if _, ok := m["content"]; ok {
			return d.interactionContent(m, "content", path)
		}
		return d.str(m, "text", path, true)
	case "thinking", "reasoning":
		if _, ok := m["text"]; ok {
			return d.str(m, "text", path, true)
		}
		return d.str(m, "thinking", path, true)
	case "image", "audio", "video", "document", "file", "image_url", "input_audio":
		return nil
	case "function_call", "function_result":
		return d.interactionStep(m, path)
	case "code_execution_call":
		return d.str(m, "code", path, true)
	case "code_execution_result":
		return d.str(m, "result", path, true)
	default:
		return errors.New("unsupported_interaction_content_node")
	}
}
func (d *Document) interactionsResponse(m map[string]any, path string) error {
	if v, ok := m["interaction"]; ok {
		x, err := object(v, path+"/interaction")
		if err != nil {
			return err
		}
		return d.interactionsResponse(x, path+"/interaction")
	}
	if err := d.reference(m, "id", "interaction", true); err != nil {
		return err
	}
	for _, key := range []string{"steps", "outputs"} {
		if v, exists := m[key]; exists {
			a, err := array(v, path+"/"+key)
			if err != nil {
				return err
			}
			for i, v := range a {
				x, err := object(v, path+"/"+key)
				if err != nil {
					return err
				}
				if err := d.interactionStep(x, pathIndex(path+"/"+key, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
