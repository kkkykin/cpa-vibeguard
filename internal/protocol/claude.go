package protocol

import (
	"encoding/json"
	"errors"
)

func (d *Document) claudeRequest() error {
	m := d.Body
	if err := rejectKeys(m, "input", "contents", "prompt"); err != nil {
		return err
	}
	if v, ok := m["system"]; ok {
		if _, ok := v.(string); ok {
			if err := d.str(m, "system", "", true); err != nil {
				return err
			}
		} else {
			a, err := array(v, "system")
			if err != nil {
				return err
			}
			for i, v := range a {
				x, err := object(v, "system")
				if err != nil {
					return err
				}
				if kind(x) != "text" {
					return errors.New("unsupported_system_block")
				}
				if err := d.str(x, "text", pathIndex("/system", i), true); err != nil {
					return err
				}
			}
		}
	}
	a, err := array(m["messages"], "messages")
	if err != nil || len(a) == 0 {
		return errors.New("messages_required")
	}
	for i, v := range a {
		x, err := object(v, "messages")
		if err != nil {
			return err
		}
		role, _ := x["role"].(string)
		if role != "user" && role != "assistant" {
			return errors.New("unsupported_message_role")
		}
		if err := d.claudeContent(x, "content", pathIndex("/messages", i)); err != nil {
			return err
		}
	}
	if v, ok := m["tools"]; ok {
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
func (d *Document) claudeContent(m map[string]any, key, path string) error {
	if _, ok := m[key].(string); ok {
		return d.str(m, key, path, true)
	}
	a, err := array(m[key], path+"/"+key)
	if err != nil {
		return err
	}
	for i, v := range a {
		x, err := object(v, path+"/"+key)
		if err != nil {
			return err
		}
		if err := d.claudeBlock(x, pathIndex(path+"/"+key, i)); err != nil {
			return err
		}
	}
	return nil
}
func (d *Document) claudeBlock(m map[string]any, path string) error {
	switch kind(m) {
	case "text":
		return d.str(m, "text", path, true)
	case "thinking":
		return d.str(m, "thinking", path, true)
	case "redacted_thinking", "image", "tool_reference", "container_upload":
		return nil
	case "document":
		for _, key := range []string{"title", "context"} {
			if err := d.str(m, key, path, false); err != nil {
				return err
			}
		}
		if v, ok := m["source"]; ok {
			x, err := object(v, path+"/source")
			if err != nil {
				return err
			}
			switch kind(x) {
			case "text":
				return d.str(x, "data", path+"/source", true)
			case "content":
				return d.claudeContent(x, "content", path+"/source")
			case "base64", "url":
				return nil
			default:
				return errors.New("unsupported_document_source")
			}
		}
		return errors.New("document_source_required")
	case "tool_use", "server_tool_use":
		return d.toolObject(m, "input", path, true)
	case "tool_result":
		return d.claudeContent(m, "content", path)
	case "search_result":
		if err := d.str(m, "title", path, false); err != nil {
			return err
		}
		return d.claudeContent(m, "content", path)
	case "web_search_tool_result":
		switch v := m["content"].(type) {
		case []any:
			for i, v := range v {
				x, err := object(v, path+"/content")
				if err != nil {
					return err
				}
				if kind(x) != "web_search_result" {
					return errors.New("unsupported_web_search_result")
				}
				for _, key := range []string{"title", "text"} {
					if err := d.str(x, key, pathIndex(path+"/content", i), false); err != nil {
						return err
					}
				}
			}
			return nil
		case map[string]any:
			if kind(v) == "web_search_tool_result_error" {
				return nil
			}
		}
		return errors.New("invalid_web_search_result")
	case "web_fetch_tool_result":
		x, err := object(m["content"], path+"/content")
		if err != nil {
			return err
		}
		if kind(x) == "web_fetch_tool_result_error" {
			return nil
		}
		if kind(x) != "web_fetch_result" {
			return errors.New("unsupported_web_fetch_result")
		}
		content, err := object(x["content"], path+"/content/content")
		if err != nil {
			return err
		}
		return d.claudeBlock(content, path+"/content/content")
	case "code_execution_tool_result", "bash_code_execution_tool_result", "text_editor_code_execution_tool_result":
		x, err := object(m["content"], path+"/content")
		if err != nil {
			return err
		}
		for _, key := range []string{"stdout", "stderr", "content", "old_str", "new_str"} {
			if _, ok := x[key].(string); ok {
				if err := d.str(x, key, path+"/content", false); err != nil {
					return err
				}
			}
		}
		return nil
	default:
		return errors.New("unsupported_claude_content_node")
	}
}
func (d *Document) claudeResponse() error {
	if v, exists := d.Body["input_tokens"]; exists {
		if _, ok := v.(json.Number); !ok {
			return errors.New("invalid_token_count")
		}
		return nil
	}
	return d.claudeContent(d.Body, "content", "")
}
