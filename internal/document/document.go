// Package document parses untrusted structured input without type coercion or
// duplicate members. Errors intentionally omit input values.
package document

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const MaxDepth = 128

func JSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid_utf8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := jsonValue(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errors.New("invalid_json_trailing_data")
	}
	return v, nil
}

func jsonValue(d *json.Decoder, depth int) (any, error) {
	if depth > MaxDepth {
		return nil, errors.New("document_depth_limit")
	}
	t, err := d.Token()
	if err != nil {
		return nil, errors.New("invalid_json")
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return t, nil
	}
	switch delim {
	case '{':
		m := make(map[string]any)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, errors.New("invalid_json")
			}
			s, ok := key.(string)
			if !ok {
				return nil, errors.New("invalid_json_member")
			}
			if _, exists := m[s]; exists {
				return nil, errors.New("duplicate_json_member")
			}
			v, err := jsonValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			m[s] = v
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("invalid_json")
		}
		return m, nil
	case '[':
		a := make([]any, 0)
		for d.More() {
			v, err := jsonValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("invalid_json")
		}
		return a, nil
	default:
		return nil, errors.New("invalid_json")
	}
}

func YAML(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid_utf8")
	}
	d := yaml.NewDecoder(bytes.NewReader(raw))
	var n yaml.Node
	if err := d.Decode(&n); err != nil {
		return nil, errors.New("invalid_yaml")
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err != io.EOF {
		return nil, errors.New("yaml_requires_one_document")
	}
	if len(n.Content) != 1 {
		return nil, errors.New("yaml_requires_mapping")
	}
	v, err := yamlValue(n.Content[0], 0)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("yaml_requires_mapping")
	}
	return m, nil
}

func yamlValue(n *yaml.Node, depth int) (any, error) {
	fail := func(code string) (any, error) { return nil, fmt.Errorf("%s at line %d", code, n.Line) }
	if depth > MaxDepth {
		return fail("document_depth_limit")
	}
	switch n.Kind {
	case yaml.MappingNode:
		m := make(map[string]any)
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fail("yaml_requires_string_key")
			}
			if _, exists := m[key.Value]; exists {
				return fail("duplicate_yaml_member")
			}
			v, err := yamlValue(n.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			m[key.Value] = v
		}
		return m, nil
	case yaml.SequenceNode:
		a := make([]any, 0, len(n.Content))
		for _, item := range n.Content {
			v, err := yamlValue(item, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		return a, nil
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str":
			return n.Value, nil
		case "!!null":
			return nil, nil
		case "!!bool":
			var v bool
			if n.Decode(&v) != nil {
				return fail("invalid_yaml_boolean")
			}
			return v, nil
		case "!!int":
			var v int64
			if n.Decode(&v) != nil {
				return fail("invalid_yaml_integer")
			}
			return json.Number(strconv.FormatInt(v, 10)), nil
		case "!!float":
			var v float64
			if n.Decode(&v) != nil {
				return fail("invalid_yaml_number")
			}
			raw, err := json.Marshal(v)
			if err != nil {
				return fail("invalid_yaml_number")
			}
			return json.Number(raw), nil
		default:
			return fail("unsupported_yaml_tag")
		}
	default:
		return fail("unsupported_yaml_alias")
	}
}

// Decode rejects unknown fields and all scalar coercions. The input must have
// already passed JSON or YAML so duplicate members cannot be lost here.
func Decode(v any, dst any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return errors.New("invalid_document_value")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(dst); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return fmt.Errorf("invalid_field_type: %s", te.Field)
		}
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return errors.New(strings.TrimPrefix(err.Error(), "json: "))
		}
		return errors.New("invalid_document_fields")
	}
	return nil
}

func PointerParts(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("invalid_json_pointer")
	}
	parts := strings.Split(pointer[1:], "/")
	for i, part := range parts {
		for j := 0; j < len(part); j++ {
			if part[j] == '~' {
				if j+1 >= len(part) || (part[j+1] != '0' && part[j+1] != '1') {
					return nil, errors.New("invalid_json_pointer")
				}
				j++
			}
		}
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

// Pointer distinguishes an absent member from a present null or wrong type.
func Pointer(v any, pointer string) (any, bool, error) {
	parts, err := PointerParts(pointer)
	if err != nil {
		return nil, false, err
	}
	for _, part := range parts {
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = x[part]
			if !ok {
				return nil, false, nil
			}
		case []any:
			if part == "" || (len(part) > 1 && part[0] == '0') {
				return nil, false, errors.New("invalid_json_pointer_index")
			}
			for _, c := range part {
				if c < '0' || c > '9' {
					return nil, false, errors.New("invalid_json_pointer_index")
				}
			}
			n, err := strconv.Atoi(part)
			if err != nil || n >= len(x) {
				return nil, false, nil
			}
			v = x[n]
		default:
			return nil, false, errors.New("json_pointer_container_type")
		}
	}
	return v, true, nil
}
