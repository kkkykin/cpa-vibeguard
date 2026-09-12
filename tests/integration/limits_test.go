//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHostOversizedRequestRejectedBeforeUpstream(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil || bytes.Contains(raw, []byte("PRIVATE")) {
			t.Error("unprotected request reached upstream")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	h := startHost(t, upstream.URL, hostRule)
	for _, tc := range []struct {
		megabytes, status int
		code              string
	}{
		{9, http.StatusBadRequest, "request_size_limit"},
		{49, http.StatusRequestEntityTooLarge, "rpc_size_limit"},
	} {
		t.Run(fmt.Sprintf("%dMiB", tc.megabytes), func(t *testing.T) {
			body := `{"model":"fixture-chat","messages":[{"role":"user","content":"` + strings.Repeat("x", tc.megabytes<<20) + ` PRIVATE"}]}`
			status, raw := post(t, h.url+"/v1/chat/completions", body, false)
			if status != tc.status || !bytes.Contains(raw, []byte(tc.code)) || calls.Load() != 0 {
				t.Fatalf("oversized request: HTTP %d, upstream calls %d, response %s", status, calls.Load(), raw)
			}
		})
	}
	status, raw := post(t, h.url+"/v1/chat/completions", `{"model":"fixture-chat","messages":[{"role":"user","content":"PRIVATE"}]}`, false)
	if status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("normal request after rejections: HTTP %d, calls %d, response %s", status, calls.Load(), raw)
	}
}
