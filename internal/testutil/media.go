// Package testutil provides synthetic media for preservation regressions.
package testutil

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"strings"
	"testing"
)

// A decodable PNG with a private ancillary chunk whose base64 happens to
// contain a phone-shaped string. This tests the repository's actual PII rule,
// without creating a rule that deliberately targets an image signature.
func PNGWithPhone(t testing.TB) ([]byte, string) {
	t.Helper()
	var original bytes.Buffer
	if err := png.Encode(&original, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	raw := original.Bytes()
	encodedFragment := "/13800138000/AAA"
	fragment, err := base64.RawStdEncoding.DecodeString(encodedFragment)
	if err != nil {
		t.Fatal(err)
	}
	pad := (3 - ((len(raw) - 12 + 8) % 3)) % 3
	payload := append(make([]byte, pad), fragment...)
	chunk := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(chunk, uint32(len(payload)))
	copy(chunk[4:8], "vpAg")
	copy(chunk[8:], payload)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	out := append([]byte(nil), raw[:len(raw)-12]...)
	out = append(out, chunk...)
	out = append(out, raw[len(raw)-12:]...)
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(out)
	if !strings.Contains(b64, "/13800138000/") {
		t.Fatal("fixture alignment failed")
	}
	return out, b64
}
