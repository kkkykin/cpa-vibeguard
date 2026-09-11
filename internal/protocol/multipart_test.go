package protocol

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
)

func TestMultipartPromptAndBinaryPreservation(t *testing.T) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("model", "PRIVATE_MODEL")
	w.WriteField("prompt", "PRIVATE 中文")
	w.WriteField("negative_prompt", "PRIVATE")
	file, _ := w.CreateFormFile("image[]", "PRIVATE.png")
	binary := []byte{0, 255, 1, 13, 10, '[', '[', 'P', 'R', 'I', 'V', 'A', 'T', 'E', ']', ']'}
	file.Write(binary)
	w.Close()
	headers := http.Header{"Content-Type": {w.FormDataContentType()}}
	d, err := ParseRequest("openai-image", body.Bytes(), headers, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if d.Root != nil || len(d.Fields) != 2 {
		t.Fatal("multipart invented JSON sources or selected binary data")
	}
	for _, f := range d.Fields {
		f.Set("masked")
	}
	d.AppendHint()
	out, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	r := multipart.NewReader(bytes.NewReader(out), w.Boundary())
	values := map[string]string{}
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		if p.FileName() != "" {
			if p.FileName() != "PRIVATE.png" || !bytes.Equal(binary, b) {
				t.Fatal("binary/filename changed")
			}
		} else {
			values[p.FormName()] = string(b)
		}
	}
	if values["model"] != "PRIVATE_MODEL" || values["prompt"] != "masked" || values["negative_prompt"] != "masked" {
		t.Fatal(values)
	}
	// xAI/Codex handlers can normalize multipart uploads before the hook.
	if _, err := ParseRequest("openai-image", []byte(`{"prompt":"PRIVATE","image":"opaque"}`), headers, 1024); err != nil {
		t.Fatal(err)
	}
}
func TestMultipartAmbiguityAndInvalidBodies(t *testing.T) {
	for _, which := range []string{"duplicate", "file-prompt", "bad-utf8", "incomplete", "missing"} {
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		if which != "missing" {
			w.WriteField("prompt", "PRIVATE")
		}
		switch which {
		case "duplicate":
			w.WriteField("prompt", "other")
		case "file-prompt":
			p, _ := w.CreateFormFile("prompt", "prompt.txt")
			p.Write([]byte("PRIVATE"))
		case "bad-utf8":
			w.WriteField("negative_prompt", string([]byte{255}))
		}
		if which != "incomplete" {
			w.Close()
		}
		if _, err := ParseRequest("openai-image", body.Bytes(), http.Header{"Content-Type": {w.FormDataContentType()}}, 1024); err == nil {
			t.Fatal("accepted", which)
		}
	}
	if _, err := ParseRequest("openai-image", []byte("--boundary"), http.Header{"Content-Type": {"multipart/form-data; boundary=x", "multipart/form-data; boundary=y"}}, 1024); err == nil {
		t.Fatal("ambiguous Content-Type accepted")
	}
}
