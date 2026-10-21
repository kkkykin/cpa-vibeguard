package rules

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"mime"
	"net/url"
	"strings"
)

// encodedMedia selects opaque islands before running rules. Data URIs declare
// their encoding; unlabelled base64 must also have a recognizable file header.
// Merely looking like base64 is not sufficient to exempt an API key or prose.
// Payloads are never decoded for secret scanning or expanded into an image.
func encodedMedia(text string) []Span {
	// MIME encoders may wrap a standalone file value at CR/LF. Only treat the
	// whole value this way: a newline beside an embedded image can be prose.
	trimmed := strings.TrimSpace(text)
	if len(trimmed) >= 32 && wrappedBase64(trimmed) {
		compact := unwrappedBase64(trimmed)
		if base64End(compact, 0) == len(compact) {
			if enc := base64Encoding(compact); enc != nil && len(compact) >= 32 && binaryHeader(compact, enc) {
				start := strings.Index(text, trimmed)
				return []Span{{Start: start, End: start + len(trimmed)}}
			}
		}
	}
	var spans []Span
	for i := 0; i < len(text); {
		if end := dataURIEnd(text, i); end > i {
			spans = append(spans, Span{Start: i, End: end})
			if len(spans) > MaxScanMatches {
				return spans
			}
			i = end
			continue
		}
		end := base64End(text, i)
		if end == i {
			i++
			continue
		}
		if end-i >= 32 {
			if enc := base64Encoding(text[i:end]); enc != nil && binaryHeader(text[i:end], enc) {
				spans = append(spans, Span{Start: i, End: end})
				if len(spans) > MaxScanMatches {
					return spans
				}
			}
		}
		i = end
	}
	return spans
}

func base64Char(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '-' || c == '_'
}

func base64End(text string, start int) int {
	i := start
	for i < len(text) && base64Char(text[i]) {
		i++
	}
	if i == start {
		return start
	}
	for n := 0; n < 2 && i < len(text) && text[i] == '='; n++ {
		i++
	}
	return i
}

// The caller has already checked the alphabet with base64End. Validate the
// final quantum and padding without allocating a decoded copy of the payload.
func base64Encoding(s string) *base64.Encoding {
	if len(s) == 0 {
		return nil
	}
	enc := base64.StdEncoding
	if strings.ContainsAny(s, "-_") {
		if strings.ContainsAny(s, "+/") {
			return nil
		}
		enc = base64.URLEncoding
	}
	n := len(s) % 4
	if strings.HasSuffix(s, "=") {
		if n != 0 {
			return nil
		}
	} else {
		enc = enc.WithPadding(base64.NoPadding)
	}
	if n == 0 {
		n = 4
	}
	var tail [3]byte
	if _, err := enc.Strict().Decode(tail[:], []byte(s[len(s)-n:])); err != nil {
		return nil
	}
	return enc
}

func dataURIEnd(text string, start int) int {
	if len(text)-start < 13 || !strings.EqualFold(text[start:start+5], "data:") {
		return start
	}
	// A URI starts at a token boundary, not in identifiers such as metadata:.
	if start > 0 && (base64Char(text[start-1]) || text[start-1] == '.') {
		return start
	}
	comma := start + 5
	for comma < len(text) && text[comma] != ',' {
		c := text[comma]
		if comma-start > 4096 || c <= ' ' || c >= 127 || strings.ContainsRune("\"'<>[]():", rune(c)) {
			return start
		}
		comma++
	}
	if comma == len(text) {
		return start
	}
	header := text[start+5 : comma]
	if len(header) < 7 || !strings.EqualFold(header[len(header)-7:], ";base64") {
		return start
	}
	header = header[:len(header)-7]
	if header == "" || header[0] == ';' {
		header = "text/plain" + header
	}
	typ, _, err := mime.ParseMediaType(header)
	if err != nil || !strings.Contains(typ, "/") {
		return start
	}
	end := comma + 1
	for end < len(text) {
		if base64Char(text[end]) || text[end] == '=' {
			end++
		} else if text[end] == '%' && end+2 < len(text) {
			end += 3
		} else {
			break
		}
	}
	payload := text[comma+1 : end]
	if strings.Contains(payload, "%") {
		payload, err = url.PathUnescape(payload)
		if err != nil {
			return start
		}
		payload = unwrappedBase64(payload)
	}
	if base64End(payload, 0) != len(payload) || base64Encoding(payload) == nil {
		return start
	}
	return end
}

func unwrappedBase64(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	return strings.ReplaceAll(s, "\n", "")
}

func wrappedBase64(s string) bool {
	wrapped := false
	for i := range len(s) {
		if s[i] == '\r' || s[i] == '\n' {
			wrapped = true
		} else if !base64Char(s[i]) && s[i] != '=' {
			return false
		}
	}
	return wrapped
}

func binaryHeader(s string, enc *base64.Encoding) bool {
	// A small prefix is enough for file signatures. Never decode or decompress
	// the complete file, even when a tool argument contains a large upload.
	n := min(len(s)/4*4, 64)
	var buf [48]byte
	n, err := enc.Decode(buf[:], []byte(s[:n]))
	if err != nil {
		return false
	}
	header := buf[:n]
	switch {
	case bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n")),
		bytes.HasPrefix(header, []byte("\xff\xd8\xff")),
		bytes.HasPrefix(header, []byte("GIF87a")), bytes.HasPrefix(header, []byte("GIF89a")):
		return true
	case len(header) >= 16 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WEBP":
		return true
	case len(header) >= 26 && string(header[:2]) == "BM":
		return binary.LittleEndian.Uint32(header[2:6]) >= 26 && bytes.Equal(header[6:10], []byte{0, 0, 0, 0}) && binary.LittleEndian.Uint32(header[14:18]) >= 12
	case bytes.HasPrefix(header, []byte("II*\x00")):
		return binary.LittleEndian.Uint32(header[4:8]) >= 8
	case bytes.HasPrefix(header, []byte("MM\x00*")):
		return binary.BigEndian.Uint32(header[4:8]) >= 8
	case bytes.HasPrefix(header, []byte{0, 0, 1, 0}):
		return binary.LittleEndian.Uint16(header[4:6]) > 0
	case len(header) >= 16 && string(header[4:8]) == "ftyp":
		switch string(header[8:12]) {
		case "avif", "avis", "heic", "heix", "hevc", "hevx", "mif1", "msf1":
			return true
		}
	case bytes.HasPrefix(header, []byte("%PDF-")):
		return header[5] >= '0' && header[5] <= '9' && header[6] == '.' && header[7] >= '0' && header[7] <= '9'
	}
	return false
}
