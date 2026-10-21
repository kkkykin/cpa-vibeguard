package rules

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"net/url"
	"strings"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/testutil"
)

func TestBareEncodedImageFormats(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	png, _ := testutil.PNGWithPhone(t)
	var jpegData, gifData bytes.Buffer
	if err := jpeg.Encode(&jpegData, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifData, img, nil); err != nil {
		t.Fatal(err)
	}
	e := testEngine(t, custom("        - {id: broad, regex: '[A-Za-z0-9+/=]+'}\n"))
	for _, raw := range [][]byte{png, jpegData.Bytes(), gifData.Bytes()} {
		encoded := base64.StdEncoding.EncodeToString(raw)
		text := "PRIVATE " + encoded + " PRIVATE"
		spans := scan(t, e, text, Input{})
		if len(spans) != 2 {
			t.Fatal("image was scanned", spans)
		}
		for _, span := range spans {
			if text[span.Start:span.End] != "PRIVATE" {
				t.Fatal("image payload selected")
			}
		}
	}
}

func TestMediaPreservesPayloadAndSurroundingSecretOffsets(t *testing.T) {
	raw, png := testutil.PNGWithPhone(t)
	e := testEngine(t, custom("        - {id: phone, regex: '\\b1[3-9][0-9]{9}\\b'}\n"))
	for _, media := range []string{
		"![image](data:image/png;base64," + png + ")",
		"<img src=\"DATA:image/png;charset=utf-8;BASE64," + png + "\">",
		"data:;base64," + png,
		"data:application/octet-stream;base64," + png,
		"data:image/png;base64," + url.PathEscape(png),
		png,
		base64.RawStdEncoding.EncodeToString(raw),
		base64.URLEncoding.EncodeToString(raw),
		base64.RawURLEncoding.EncodeToString(raw),
	} {
		t.Run(fmt.Sprint(len(media), media[:min(20, len(media))]), func(t *testing.T) {
			text := "中文 13800138000\r\n" + media + "\n13800138000 尾部"
			spans := scan(t, e, text, Input{})
			if len(spans) != 2 || spans[0].Start != strings.Index(text, "13800138000") || spans[1].Start != strings.LastIndex(text, "13800138000") {
				t.Fatalf("selected encoded bytes or lost surrounding text: %+v", spans)
			}
			for _, span := range spans {
				if text[span.Start:span.End] != "13800138000" {
					t.Fatal("incorrect original byte offsets")
				}
			}
		})
	}
}

func TestWrappedStandaloneBase64Image(t *testing.T) {
	_, png := testutil.PNGWithPhone(t)
	var wrapped strings.Builder
	for i := 0; i < len(png); i += 64 {
		wrapped.WriteString(png[i:min(i+64, len(png))])
		wrapped.WriteString("\r\n")
	}
	e := testEngine(t, custom("        - {id: phone, regex: '\\b1[3-9][0-9]{9}\\b'}\n"))
	for _, text := range []string{wrapped.String(), "data:image/png;base64," + url.PathEscape(wrapped.String())} {
		if spans := scan(t, e, text, Input{}); len(spans) != 0 {
			t.Fatal("wrapped payload was scanned", spans)
		}
	}
}

func TestBroadRulesCannotCrossMedia(t *testing.T) {
	_, png := testutil.PNGWithPhone(t)
	media := "data:image/png;base64," + png
	text := "PRIVATE before\n" + media + "\nPRIVATE after"
	e := testEngine(t, custom("        - {id: broad, regex: '(?s)PRIVATE.*'}\n"))
	spans := scan(t, e, text, Input{})
	if len(spans) != 2 {
		t.Fatal(spans)
	}
	for _, s := range spans {
		if s.Start < strings.Index(text, media)+len(media) && s.End > strings.Index(text, media) {
			t.Fatal("broad match covered the media")
		}
	}
}

func TestBase64LookingKeysAndMalformedURIsStillScan(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEF"))
	e := testEngine(t, custom("        - {id: key, regex: '"+key+"'}\n        - {id: text, regex: PRIVATE}\n"))
	for _, text := range []string{
		key, "api_key=" + key,
		"metadata:;base64," + key,
		"data:invalid;base64," + key,
		"data:text/plain,PRIVATE",
		"data:image/png;base64,PRIVATE!PRIVATE",
	} {
		if spans := scan(t, e, text, Input{}); len(spans) == 0 {
			t.Fatalf("ordinary secret was exempted: %q", text)
		}
	}
	// The default detector must still see real-looking credentials.
	e = testEngine(t, "")
	if spans := scan(t, e, "github=ghp_0123456789abcdef0123456789ABCDEFGHijkl", Input{}); len(spans) == 0 {
		t.Fatal("default credential detection regressed")
	}
}

func TestMediaScanLimitsExcludePayloadAndCountAllText(t *testing.T) {
	e := testEngine(t, custom("        - {id: text, regex: 'A|PRIVATE'}\n"))
	text := "PRIVATE data:application/octet-stream;base64," + strings.Repeat("A", 4*(MaxScanMatches+1)) + " PRIVATE"
	if spans := scan(t, e, text, Input{}); len(spans) != 2 {
		t.Fatal(spans)
	}
	e = testEngine(t, custom("        - {id: text, regex: x}\n"))
	b, err := e.Bind(Input{})
	if err != nil {
		t.Fatal(err)
	}
	text = strings.Repeat("x ", MaxScanMatches/2) + "data:;base64,QUJD " + strings.Repeat("x ", MaxScanMatches/2+1)
	if spans, err := b.Scan(context.Background(), text); err == nil || len(spans) != 0 {
		t.Fatal("splitting at media reset the text match limit")
	}
	text = strings.Repeat("data:;base64,QUJD ", MaxScanMatches+1)
	if spans, err := b.Scan(context.Background(), text); err == nil || len(spans) != 0 {
		t.Fatal("unbounded number of media islands accepted")
	}
}

func FuzzEncodedMediaRanges(f *testing.F) {
	f.Add("data:image/png;base64,QUJDRA== PRIVATE")
	f.Add("data:;base64,A")
	f.Add("data:;base64,AAAA=AAAA====")
	f.Add("iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB")
	f.Fuzz(func(t *testing.T, text string) {
		previous := 0
		for _, span := range encodedMedia(text) {
			if span.Start < previous || span.End <= span.Start || span.End > len(text) {
				t.Fatalf("invalid range: %+v", span)
			}
			previous = span.End
		}
	})
}
