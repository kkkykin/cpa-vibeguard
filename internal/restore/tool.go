package restore

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/kkkykin/cpa-vibeguard/internal/document"
)

type toolFrame struct{ kind, phase byte }
type ToolJSON struct {
	lookup                   Lookup
	limit, count             int
	raw                      strings.Builder
	partial                  string
	stack                    []toolFrame
	started, done, validated bool
	inString, key            bool
	primitive                bool
	text                     *Text
}

func NewToolJSON(lookup Lookup, limit int) *ToolJSON {
	return &ToolJSON{lookup: lookup, limit: limit, text: NewText(lookup)}
}
func (c *ToolJSON) Buffered() int {
	return c.raw.Cap() + len(c.partial) + c.text.Buffered() + cap(c.stack)*2
}
func (c *ToolJSON) Abort() {
	c.raw.Reset()
	c.partial = ""
	c.stack = nil
	c.text.Abort()
	c.done = true
}
func (c *ToolJSON) wantValue() bool {
	if len(c.stack) == 0 {
		return !c.started
	}
	p := c.stack[len(c.stack)-1].phase
	return p == 'v' || p == 'a'
}
func (c *ToolJSON) valueDone() {
	if len(c.stack) == 0 {
		c.done = true
	} else {
		c.stack[len(c.stack)-1].phase = 'd'
	}
}
func escaped(s string) string {
	if s == "" {
		return ""
	}
	raw, _ := json.Marshal(s)
	return string(raw[1 : len(raw)-1])
}
func white(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }

// Push restores decoded string values while preserving JSON syntax and key
// names. The complete original argument JSON is separately bounded and checked
// for duplicate members and grammar before a completed value is accepted.
func (c *ToolJSON) Push(fragment string, final bool) (string, error) {
	c.count += len(fragment)
	if c.count > c.limit {
		return "", errors.New("tool_json_size_limit")
	}
	if !c.validated {
		c.raw.WriteString(fragment)
	}
	input := c.partial + fragment
	c.partial = ""
	var out strings.Builder
	for i := 0; i < len(input); {
		b := input[i]
		if c.inString {
			if b == '"' {
				if !c.key {
					out.WriteString(escaped(c.text.Push("", true)))
				}
				out.WriteByte(b)
				i++
				c.inString = false
				if c.key {
					c.stack[len(c.stack)-1].phase = 'c'
				} else {
					c.valueDone()
				}
				continue
			}
			var value string
			consumed := 1
			if b == '\\' {
				var incomplete bool
				var err error
				value, consumed, incomplete, err = decodeEscape(input[i:])
				if err != nil {
					return "", err
				}
				if incomplete {
					c.partial = strings.Clone(input[i:])
					break
				}
			} else if b >= utf8.RuneSelf {
				if !utf8.FullRuneInString(input[i:]) {
					c.partial = strings.Clone(input[i:])
					break
				}
				r, n := utf8.DecodeRuneInString(input[i:])
				if r == utf8.RuneError && n == 1 {
					return "", errors.New("invalid_tool_utf8")
				}
				consumed = n
				value = input[i : i+n]
			} else {
				if b < 0x20 {
					return "", errors.New("invalid_tool_json_control")
				}
				value = input[i : i+1]
			}
			if c.key {
				out.WriteString(input[i : i+consumed])
			} else {
				out.WriteString(escaped(c.text.Push(value, false)))
			}
			i += consumed
			continue
		}
		if c.primitive {
			if white(b) || b == ',' || b == '}' || b == ']' {
				c.primitive = false
				c.valueDone()
				continue
			}
			if b == '"' || b == '{' || b == '[' || b == ':' {
				return "", errors.New("invalid_tool_json_primitive")
			}
			out.WriteByte(b)
			i++
			continue
		}
		if white(b) {
			out.WriteByte(b)
			i++
			continue
		}
		if c.done {
			return "", errors.New("tool_json_trailing_data")
		}
		switch b {
		case '{', '[':
			if !c.wantValue() {
				return "", errors.New("invalid_tool_json_structure")
			}
			c.started = true
			phase := byte('k')
			if b == '[' {
				phase = 'a'
			}
			c.stack = append(c.stack, toolFrame{kind: b, phase: phase})
			if len(c.stack) > document.MaxDepth {
				return "", errors.New("tool_json_depth_limit")
			}
			out.WriteByte(b)
			i++
		case '"':
			if len(c.stack) == 0 {
				return "", errors.New("tool_json_requires_container")
			}
			top := c.stack[len(c.stack)-1]
			c.key = top.kind == '{' && (top.phase == 'k' || top.phase == 'K')
			if !c.key && !c.wantValue() {
				return "", errors.New("invalid_tool_json_structure")
			}
			c.inString = true
			out.WriteByte(b)
			i++
		case ':':
			if len(c.stack) == 0 || c.stack[len(c.stack)-1].phase != 'c' {
				return "", errors.New("invalid_tool_json_colon")
			}
			c.stack[len(c.stack)-1].phase = 'v'
			out.WriteByte(b)
			i++
		case ',':
			if len(c.stack) == 0 || c.stack[len(c.stack)-1].phase != 'd' {
				return "", errors.New("invalid_tool_json_comma")
			}
			top := &c.stack[len(c.stack)-1]
			top.phase = 'v'
			if top.kind == '{' {
				top.phase = 'K'
			}
			out.WriteByte(b)
			i++
		case '}', ']':
			if len(c.stack) == 0 {
				return "", errors.New("invalid_tool_json_close")
			}
			top := c.stack[len(c.stack)-1]
			if b == '}' && (top.kind != '{' || (top.phase != 'k' && top.phase != 'd')) || b == ']' && (top.kind != '[' || (top.phase != 'a' && top.phase != 'd')) {
				return "", errors.New("invalid_tool_json_close")
			}
			c.stack = c.stack[:len(c.stack)-1]
			out.WriteByte(b)
			i++
			c.valueDone()
		default:
			if len(c.stack) == 0 || !c.wantValue() {
				return "", errors.New("invalid_tool_json_value")
			}
			c.primitive = true
		}
	}
	if c.done && !c.validated {
		if _, err := document.JSON([]byte(c.raw.String())); err != nil {
			return "", errors.New("invalid_completed_tool_json")
		}
		c.validated = true
		c.raw.Reset()
		c.stack = nil
	}
	if final && (!c.done || c.partial != "" || c.inString || c.primitive) {
		return "", errors.New("incomplete_tool_json")
	}
	return out.String(), nil
}
func decodeEscape(s string) (string, int, bool, error) {
	if len(s) < 2 {
		return "", 0, true, nil
	}
	switch s[1] {
	case '"', '\\', '/':
		return s[1:2], 2, false, nil
	case 'b':
		return "\b", 2, false, nil
	case 'f':
		return "\f", 2, false, nil
	case 'n':
		return "\n", 2, false, nil
	case 'r':
		return "\r", 2, false, nil
	case 't':
		return "\t", 2, false, nil
	case 'u':
		if len(s) < 6 {
			return "", 0, true, nil
		}
		n, err := strconv.ParseUint(s[2:6], 16, 16)
		if err != nil {
			return "", 0, false, errors.New("invalid_tool_json_escape")
		}
		r := rune(n)
		if r >= 0xd800 && r <= 0xdbff {
			if len(s) < 12 {
				return "", 0, true, nil
			}
			if s[6:8] != "\\u" {
				return "", 0, false, errors.New("invalid_tool_json_surrogate")
			}
			low, err := strconv.ParseUint(s[8:12], 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return "", 0, false, errors.New("invalid_tool_json_surrogate")
			}
			return string(utf16.DecodeRune(r, rune(low))), 12, false, nil
		}
		if r >= 0xdc00 && r <= 0xdfff {
			return "", 0, false, errors.New("invalid_tool_json_surrogate")
		}
		return string(r), 6, false, nil
	default:
		return "", 0, false, errors.New("invalid_tool_json_escape")
	}
}
