package restore

import (
	"bytes"
	"errors"
	"strings"
)

type Frame struct {
	Data, Raw      []byte
	HasData, Close bool
	Lines          []string
	Event          string
}

func (f Frame) Render(data []byte) []byte {
	if !f.HasData {
		return f.Raw
	}
	var out bytes.Buffer
	for _, line := range f.Lines {
		out.WriteString(line)
		out.WriteByte('\n')
	}
	out.WriteString("data: ")
	out.Write(data)
	out.WriteString("\n\n")
	return out.Bytes()
}

type Framer struct {
	Limit                     int
	Mode                      string
	buffer                    []byte
	scan, depth               int
	inString, escape, started bool
	comma, afterComma, closed bool
}

func (f *Framer) Buffered() int { return len(f.buffer) }
func (f *Framer) Abort()        { f.buffer = nil; f.scan = 0; f.depth = 0; f.closed = true }
func (f *Framer) Push(chunk []byte) ([]Frame, error) {
	if f.closed && len(bytes.TrimSpace(chunk)) > 0 {
		return nil, errors.New("stream_after_envelope_end")
	}
	f.buffer = append(f.buffer, chunk...)
	if f.Mode == "" {
		trim := bytes.TrimSpace(f.buffer)
		if len(trim) == 0 {
			if len(f.buffer) > f.Limit {
				return nil, errors.New("event_size_limit")
			}
			return nil, nil
		}
		switch trim[0] {
		case '{':
			f.Mode = "json"
			f.buffer = bytes.TrimLeft(f.buffer, " \t\r\n")
		case '[':
			f.Mode = "array"
			f.buffer = trim[1:]
		default:
			for _, prefix := range []string{"data:", "event:", "id:", "retry:", ":"} {
				if bytes.HasPrefix(trim, []byte(prefix)) {
					f.Mode = "sse"
					break
				}
				if bytes.HasPrefix([]byte(prefix), trim) {
					return nil, nil
				}
			}
			if f.Mode == "" {
				if i := bytes.IndexByte(trim, ':'); i > 0 {
					f.Mode = "sse"
				} else {
					return nil, errors.New("unsupported_stream_framing")
				}
			}
		}
	}
	var frames []Frame
	for {
		if f.Mode == "sse" {
			end := -1
			for i := f.scan; i < len(f.buffer); i++ {
				if f.buffer[i] == '\n' && (i == 0 || f.buffer[i-1] == '\n' || (i >= 2 && f.buffer[i-1] == '\r' && f.buffer[i-2] == '\n') || (i == 1 && f.buffer[0] == '\r')) {
					end = i + 1
					break
				}
			}
			if end < 0 {
				f.scan = max(0, len(f.buffer)-2)
				break
			}
			if end > f.Limit {
				return nil, errors.New("event_size_limit")
			}
			frames = append(frames, parseSSE(f.buffer[:end]))
			f.buffer = f.buffer[end:]
			f.scan = 0
			continue
		}
		if !f.started {
			f.buffer = bytes.TrimLeft(f.buffer, " \t\r\n")
			if len(f.buffer) == 0 {
				break
			}
			if f.Mode == "array" {
				if f.buffer[0] == ']' {
					if f.afterComma {
						return nil, errors.New("invalid_json_stream_array")
					}
					frames = append(frames, Frame{Close: true})
					f.buffer = f.buffer[1:]
					f.closed = true
					if len(bytes.TrimSpace(f.buffer)) > 0 {
						return nil, errors.New("stream_after_envelope_end")
					}
					f.buffer = nil
					break
				}
				if f.comma {
					if f.buffer[0] != ',' {
						return nil, errors.New("invalid_json_stream_array")
					}
					f.buffer = f.buffer[1:]
					f.comma = false
					f.afterComma = true
					continue
				}
			}
			if f.buffer[0] != '{' {
				return nil, errors.New("stream_event_requires_object")
			}
			f.started = true
			f.scan = 0
			f.depth = 0
			f.inString = false
			f.escape = false
		}
		end := -1
		for i := f.scan; i < len(f.buffer); i++ {
			c := f.buffer[i]
			if f.inString {
				if f.escape {
					f.escape = false
				} else if c == '\\' {
					f.escape = true
				} else if c == '"' {
					f.inString = false
				}
			} else {
				switch c {
				case '"':
					f.inString = true
				case '{', '[':
					f.depth++
				case '}', ']':
					f.depth--
					if f.depth == 0 {
						end = i + 1
					}
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			f.scan = len(f.buffer)
			break
		}
		if end > f.Limit {
			return nil, errors.New("event_size_limit")
		}
		frames = append(frames, Frame{Data: bytes.Clone(f.buffer[:end]), HasData: true})
		f.buffer = f.buffer[end:]
		f.scan = 0
		f.started = false
		f.comma = true
		f.afterComma = false
	}
	if len(f.buffer) > f.Limit {
		return nil, errors.New("event_size_limit")
	}
	if len(f.buffer) == 0 {
		f.buffer = nil
	} else {
		f.buffer = bytes.Clone(f.buffer)
	}
	return frames, nil
}
func parseSSE(raw []byte) Frame {
	f := Frame{Raw: bytes.Clone(raw)}
	var data []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		if key == "data" {
			data = append(data, value)
			f.HasData = true
		} else {
			f.Lines = append(f.Lines, line)
			if key == "event" {
				f.Event = value
			}
		}
	}
	if f.HasData {
		f.Data = []byte(strings.Join(data, "\n"))
	}
	return f
}
