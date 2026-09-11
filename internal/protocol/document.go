// Package protocol selects field codecs strictly by the host's SourceFormat.
// It never guesses a protocol from model names, providers, or JSON contents.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kkkykin/cpa-vibeguard/internal/document"
)

var ErrUnknown = errors.New("unsupported_api")

const Hint = "Preserve privacy markers exactly as written: [[redacted:v1:<id>]]. Copy both brackets, the full prefix, and all 32 lowercase hexadecimal ID characters; do not shorten, reformat, or invent markers."
const MaxFields = 32768

type Field struct {
	Path, Text string
	set        func(string)
}

func (f Field) Set(s string) { f.set(s) }

type Reference struct{ Kind, ID string }
type codec struct {
	tree   any
	parent map[string]any
	key    string
}
type Document struct {
	Root        map[string]any
	Body        map[string]any
	Format      string
	Fields      []Field
	Previous    []Reference
	ResponseIDs []Reference
	toolLimit   int
	codecs      []codec
}

func Known(format string) bool {
	switch format {
	case "openai", "openai-response", "claude", "gemini", "interactions", "codex", "antigravity", "openai-image", "openai-video", "codex-alpha-search":
		return true
	}
	return false
}
func Parse(format string, raw []byte, request bool, toolLimit int) (*Document, error) {
	if !Known(format) {
		return nil, ErrUnknown
	}
	v, err := document.JSON(raw)
	if err != nil {
		return nil, err
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("protocol_requires_object")
	}
	d := &Document{Root: root, Body: root, Format: format, toolLimit: toolLimit}
	if format == "antigravity" {
		key := "request"
		if !request {
			key = "response"
		}
		if nested, exists := root[key]; exists {
			d.Body, err = object(nested, key)
			if err != nil {
				return nil, err
			}
		} else if request {
			return nil, errors.New("antigravity_request_wrapper_missing")
		}
	}
	if format == "gemini" && request {
		if nested, exists := root["generateContentRequest"]; exists {
			if _, mixed := root["contents"]; mixed {
				return nil, errors.New("mixed_protocol_structure")
			}
			d.Body, err = object(nested, "generateContentRequest")
			if err != nil {
				return nil, err
			}
		}
	}
	if request {
		switch format {
		case "openai":
			err = d.chatRequest()
		case "openai-response", "codex":
			err = d.responsesRequest()
		case "claude":
			err = d.claudeRequest()
		case "gemini", "antigravity":
			err = d.geminiRequest()
		case "interactions":
			err = d.interactionsRequest()
		default:
			err = d.specialRequest()
		}
	} else {
		switch format {
		case "openai":
			err = d.chatResponse()
		case "openai-response", "codex":
			err = d.responsesResponse(d.Body, "")
		case "claude":
			err = d.claudeResponse()
		case "gemini", "antigravity":
			err = d.geminiResponse()
		case "interactions":
			err = d.interactionsResponse(d.Body, "")
		default:
			err = d.specialResponse()
		}
	}
	if err != nil {
		return nil, err
	}
	if len(d.Fields) > MaxFields {
		return nil, errors.New("protocol_field_limit")
	}
	return d, nil
}
func (d *Document) Marshal() ([]byte, error) {
	for _, c := range d.codecs {
		raw, err := json.Marshal(c.tree)
		if err != nil {
			return nil, errors.New("tool_json_encode_error")
		}
		if len(raw) > d.toolLimit {
			return nil, errors.New("tool_json_size_limit")
		}
		c.parent[c.key] = string(raw)
	}
	raw, err := json.Marshal(d.Root)
	if err != nil {
		return nil, errors.New("protocol_encode_error")
	}
	return raw, nil
}
func object(v any, path string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("object_required: %s", path)
	}
	return m, nil
}
func array(v any, path string) ([]any, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("array_required: %s", path)
	}
	return a, nil
}
func textValue(m map[string]any, key string) (string, bool, error) {
	v, exists := m[key]
	if !exists || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, fmt.Errorf("string_required: %s", key)
	}
	return s, true, nil
}
func kind(m map[string]any) string {
	v, exists := m["type"]
	if !exists {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return "invalid-type"
	}
	return s
}
func pathIndex(path string, i int) string { return path + "/" + strconv.Itoa(i) }
func (d *Document) str(m map[string]any, key, path string, required bool) error {
	v, exists := m[key]
	if !exists || v == nil {
		if required {
			return fmt.Errorf("text_required: %s/%s", path, key)
		}
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("string_required: %s/%s", path, key)
	}
	d.Fields = append(d.Fields, Field{Path: path + "/" + key, Text: s, set: func(s string) { m[key] = s }})
	return nil
}
func (d *Document) strings(v any, set func(any), path string) error {
	switch x := v.(type) {
	case string:
		d.Fields = append(d.Fields, Field{Path: path, Text: x, set: func(s string) { set(s) }})
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := d.strings(x[k], func(v any) { x[k] = v }, path+"/"+k); err != nil {
				return err
			}
		}
	case []any:
		for i := range x {
			if err := d.strings(x[i], func(v any) { x[i] = v }, pathIndex(path, i)); err != nil {
				return err
			}
		}
	case json.Number, bool, nil:
	default:
		return errors.New("unsupported_tool_json_value")
	}
	return nil
}
func (d *Document) toolObject(m map[string]any, key, path string, required bool) error {
	v, exists := m[key]
	if !exists {
		if required {
			return fmt.Errorf("tool_object_required: %s/%s", path, key)
		}
		return nil
	}
	if _, ok := v.(map[string]any); !ok {
		return fmt.Errorf("tool_object_required: %s/%s", path, key)
	}
	raw, _ := json.Marshal(v)
	if len(raw) > d.toolLimit {
		return errors.New("tool_json_size_limit")
	}
	return d.strings(v, func(v any) { m[key] = v }, path+"/"+key)
}
func (d *Document) toolJSON(m map[string]any, key, path string) error {
	v, exists, err := textValue(m, key)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("tool_json_required: %s/%s", path, key)
	}
	if len(v) > d.toolLimit {
		return errors.New("tool_json_size_limit")
	}
	tree, err := document.JSON([]byte(v))
	if err != nil {
		return errors.New("invalid_tool_json")
	}
	switch tree.(type) {
	case map[string]any, []any:
	default:
		return errors.New("tool_json_requires_container")
	}
	c := codec{tree: tree, parent: m, key: key}
	d.codecs = append(d.codecs, c)
	return d.strings(tree, func(any) {}, path+"/"+key)
}
func (d *Document) toolResult(m map[string]any, key, path string) error {
	v, exists := m[key]
	if !exists {
		return fmt.Errorf("tool_result_required: %s/%s", path, key)
	}
	return d.strings(v, func(v any) { m[key] = v }, path+"/"+key)
}
func (d *Document) schema(v any, path string) error {
	if _, ok := v.(bool); ok {
		return nil
	}
	m, err := object(v, path)
	if err != nil {
		return err
	}
	for _, key := range []string{"description", "title"} {
		if err := d.str(m, key, path, false); err != nil {
			return err
		}
	}
	for _, key := range []string{"properties", "$defs", "definitions", "patternProperties"} {
		if v, ok := m[key]; ok {
			defs, err := object(v, path+"/"+key)
			if err != nil {
				return err
			}
			keys := make([]string, 0, len(defs))
			for k := range defs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if err := d.schema(defs[k], path+"/"+key+"/"+k); err != nil {
					return err
				}
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties", "contains", "not", "if", "then", "else"} {
		if v, ok := m[key]; ok {
			if a, ok := v.([]any); ok {
				for i, v := range a {
					if err := d.schema(v, pathIndex(path+"/"+key, i)); err != nil {
						return err
					}
				}
			} else if err := d.schema(v, path+"/"+key); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if v, ok := m[key]; ok {
			a, err := array(v, path+"/"+key)
			if err != nil {
				return err
			}
			for i, v := range a {
				if err := d.schema(v, pathIndex(path+"/"+key, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (d *Document) toolDefinition(m map[string]any, path string) error {
	if err := d.str(m, "description", path, false); err != nil {
		return err
	}
	for _, key := range []string{"parameters", "input_schema", "parametersJsonSchema"} {
		if v, ok := m[key]; ok {
			if err := d.schema(v, path+"/"+key); err != nil {
				return err
			}
		}
	}
	return nil
}
func (d *Document) reference(m map[string]any, key, kind string, output bool) error {
	id, exists, err := textValue(m, key)
	if err != nil {
		return err
	}
	if !exists || id == "" {
		return nil
	}
	r := Reference{Kind: kind, ID: id}
	if output {
		d.ResponseIDs = append(d.ResponseIDs, r)
	} else {
		d.Previous = append(d.Previous, r)
	}
	return nil
}
func rejectKeys(m map[string]any, keys ...string) error {
	for _, k := range keys {
		if _, exists := m[k]; exists {
			return errors.New("mixed_protocol_structure")
		}
	}
	return nil
}

func appendStringHint(m map[string]any, key string) {
	s, _ := m[key].(string)
	if strings.Contains(s, Hint) {
		return
	}
	if s != "" {
		s += "\n\n"
	}
	m[key] = s + Hint
}
func (d *Document) AppendHint() {
	m := d.Body
	switch d.Format {
	case "openai":
		if messages, ok := m["messages"].([]any); ok {
			for _, v := range messages {
				msg, _ := v.(map[string]any)
				role, _ := msg["role"].(string)
				s, _ := msg["content"].(string)
				if (role == "system" || role == "developer") && strings.Contains(s, Hint) {
					return
				}
			}
			m["messages"] = append([]any{map[string]any{"role": "system", "content": Hint}}, messages...)
		}
	case "openai-response", "codex":
		appendStringHint(m, "instructions")
	case "interactions":
		appendStringHint(m, "system_instruction")
	case "claude":
		if a, ok := m["system"].([]any); ok {
			for _, v := range a {
				x, _ := v.(map[string]any)
				if x["text"] == Hint {
					return
				}
			}
			m["system"] = append(a, map[string]any{"type": "text", "text": Hint})
		} else {
			appendStringHint(m, "system")
		}
	case "gemini", "antigravity":
		sys, _ := m["systemInstruction"].(map[string]any)
		if sys == nil {
			sys = make(map[string]any)
			m["systemInstruction"] = sys
		}
		parts, _ := sys["parts"].([]any)
		for _, v := range parts {
			p, _ := v.(map[string]any)
			if p["text"] == Hint {
				return
			}
		}
		sys["parts"] = append(parts, map[string]any{"text": Hint})
	}
}
