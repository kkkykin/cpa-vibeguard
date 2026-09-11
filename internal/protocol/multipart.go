package protocol

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"unicode/utf8"
)

const MaxMultipartParts = 512

type multipartPart struct {
	header textproto.MIMEHeader
	data   []byte
}
type multipartDocument struct {
	boundary string
	parts    []multipartPart
}

// ParseRequest handles the representations delivered by CPA. Image edits can
// arrive as multipart; other handlers may already have normalized that same
// HTTP Content-Type into JSON. Format selection still comes only from CPA.
func ParseRequest(format string, raw []byte, headers http.Header, toolLimit int) (*Document, error) {
	trimmed := bytes.TrimSpace(raw)
	if (format == "openai-image" || format == "openai-video") && len(trimmed) > 0 && trimmed[0] != '{' {
		var values []string
		for k, v := range headers {
			if strings.EqualFold(k, "Content-Type") {
				values = append(values, v...)
			}
		}
		if len(values) != 1 {
			return nil, errors.New("multipart_content_type_required")
		}
		mediaType, params, err := mime.ParseMediaType(values[0])
		if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
			return nil, errors.New("invalid_multipart_content_type")
		}
		return parseMultipart(format, raw, params["boundary"], toolLimit)
	}
	return Parse(format, raw, true, toolLimit)
}
func parseMultipart(format string, raw []byte, boundary string, toolLimit int) (*Document, error) {
	parts := &multipartDocument{boundary: boundary}
	d := &Document{Format: format, toolLimit: toolLimit, multipart: parts}
	reader := multipart.NewReader(bytes.NewReader(raw), boundary)
	seen := make(map[string]bool)
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("invalid_multipart_body")
		}
		if len(parts.parts) >= MaxMultipartParts {
			return nil, errors.New("multipart_part_limit")
		}
		if len(part.Header.Values("Content-Disposition")) != 1 {
			return nil, errors.New("invalid_multipart_disposition")
		}
		disposition, attrs, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil || disposition != "form-data" || attrs["name"] == "" {
			return nil, errors.New("invalid_multipart_disposition")
		}
		data, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			return nil, errors.New("invalid_multipart_part")
		}
		i := len(parts.parts)
		parts.parts = append(parts.parts, multipartPart{header: part.Header, data: data})
		name := attrs["name"]
		if name != "prompt" && name != "negative_prompt" {
			continue
		}
		if _, file := attrs["filename"]; file || seen[name] {
			return nil, errors.New("ambiguous_multipart_prompt")
		}
		seen[name] = true
		if !utf8.Valid(data) || part.Header.Get("Content-Transfer-Encoding") != "" {
			return nil, errors.New("unsupported_multipart_text_encoding")
		}
		if ct := part.Header.Get("Content-Type"); ct != "" {
			mt, params, err := mime.ParseMediaType(ct)
			if err != nil || mt != "text/plain" || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
				return nil, errors.New("unsupported_multipart_text_encoding")
			}
		}
		d.Fields = append(d.Fields, Field{Path: "/multipart/" + name, Text: string(data), set: func(v string) { parts.parts[i].data = []byte(v) }})
	}
	if !seen["prompt"] {
		return nil, errors.New("multipart_prompt_required")
	}
	return d, nil
}
func (m *multipartDocument) marshal() ([]byte, error) {
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	if err := writer.SetBoundary(m.boundary); err != nil {
		return nil, errors.New("invalid_multipart_boundary")
	}
	for _, part := range m.parts {
		p, err := writer.CreatePart(part.header)
		if err != nil {
			return nil, errors.New("multipart_encode_error")
		}
		if _, err := p.Write(part.data); err != nil {
			return nil, errors.New("multipart_encode_error")
		}
	}
	if err := writer.Close(); err != nil {
		return nil, errors.New("multipart_encode_error")
	}
	return out.Bytes(), nil
}
