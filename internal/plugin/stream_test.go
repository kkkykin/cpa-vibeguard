package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func streamRequest(t *testing.T, e *Engine, id string) string {
	t.Helper()
	r := e.Before(pluginapi.RequestInterceptRequest{RequestID: id, SourceFormat: "openai", Body: []byte(`{"messages":[{"role":"user","content":"PRIVATE"}]}`)})
	if r.Terminate {
		t.Fatalf("request rejected: %s", r.ResponseBody)
	}
	var m struct{ Messages []struct{ Content string } }
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatal(err)
	}
	return m.Messages[0].Content
}
func chatChunk(text string, finish bool) []byte {
	var reason any
	if finish {
		reason = "stop"
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": reason}}, "usage": map[string]any{"total_tokens": 3}})
	return b
}
func TestStreamHookSplitStickyConfigAndCleanup(t *testing.T) {
	e := configured(t, privateRule)
	for split := 0; split <= 48; split++ {
		id := fmt.Sprintf("split-%d", split)
		marker := streamRequest(t, e, id)
		init := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: id, SourceFormat: "openai", ChunkIndex: -1, OriginalRequest: []byte("not used")})
		if init.DropChunk || len(init.Body) != 0 {
			t.Fatal("header-init changed body")
		}
		var content string
		for i, chunk := range [][]byte{chatChunk(marker[:split], false), chatChunk(marker[split:], false), chatChunk("", true)} {
			r := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: id, SourceFormat: "openai", ChunkIndex: i, Body: chunk})
			if r.DropChunk || len(r.Body) == 0 {
				t.Fatal("lost deliverable JSON or degraded")
			}
			var m struct {
				Choices []struct{ Delta struct{ Content string } }
				Usage   map[string]any
			}
			if err := json.Unmarshal(r.Body, &m); err != nil {
				t.Fatal(err)
			}
			if m.Usage == nil {
				t.Fatal("usage lost")
			}
			content += m.Choices[0].Delta.Content
		}
		if content != "PRIVATE" {
			t.Fatal(split, content)
		}
		e.Complete(id)
		e.Complete(id)
	}
	if e.Stats().Inflight != 0 || e.Stats().ResponseDegraded != 0 {
		t.Fatal(e.Stats())
	}
	marker := streamRequest(t, e, "config")
	e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: "config", SourceFormat: "openai", Body: chatChunk(marker[:20], false)})
	if err := e.Configure([]byte(privateRule + "disabled_rules: [fixture]\n")); err != nil {
		t.Fatal(err)
	}
	r := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: "config", SourceFormat: "openai", Body: chatChunk(marker[20:], true)})
	if !bytes.Contains(r.Body, []byte("PRIVATE")) {
		t.Fatal("reconfigure lost in-flight mapping")
	}
	e.Complete("config")
}
func TestStreamDegradesOnceWithoutReplay(t *testing.T) {
	e := configured(t, privateRule)
	marker := streamRequest(t, e, "broken")
	first := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: "broken", SourceFormat: "openai", Body: chatChunk("delivered "+marker[:30], false)})
	if !bytes.Contains(first.Body, []byte("delivered ")) || bytes.Contains(first.Body, []byte(marker[:30])) {
		t.Fatal("tail leaked")
	}
	for _, body := range [][]byte{[]byte(`{"choices":],"secret":"PRIVATE"}`), chatChunk(marker, true)} {
		r := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: "broken", SourceFormat: "openai", Body: body})
		if r.DropChunk || len(r.Body) != 0 {
			t.Fatal("degradation replayed or rewrote current chunk")
		}
	}
	if e.Stats().ResponseDegraded != 1 || e.state("broken").stream != nil {
		t.Fatal(e.Stats())
	}
	e.Complete("broken")
	r := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: "missing", SourceFormat: "openai", Body: chatChunk(marker, true)})
	if len(r.Body) != 0 || r.DropChunk {
		t.Fatal("missing state searched other scopes")
	}
}
func TestStreamPartialFramingDropsAndAbortReleases(t *testing.T) {
	e := configured(t, privateRule)
	marker := streamRequest(t, e, "partial")
	body := chatChunk(marker[:30], false)
	r := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: "partial", SourceFormat: "openai", Body: body[:8]})
	if !r.DropChunk || len(r.Body) != 0 {
		t.Fatal("incomplete frame passed through")
	}
	r = e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: "partial", SourceFormat: "openai", Body: body[8:]})
	if r.DropChunk || len(r.Body) == 0 || bytes.Contains(r.Body, []byte(marker[:30])) {
		t.Fatal("field tail not held")
	}
	s := e.state("partial")
	e.Complete("partial")
	if s.stream != nil || s.lease != nil || len(s.body) != 0 {
		t.Fatal("complete retained stream state")
	}
}
func TestStreamResponseIDsPermitContinuation(t *testing.T) {
	e := configured(t, privateRule+"session:\n  profiles:\n    - namespace: history\n      sources: [{json: /metadata/session_id}]\n")
	req := pluginapi.RequestInterceptRequest{RequestID: "first", SourceFormat: "openai-response", Metadata: map[string]any{"caller_scope": "authenticated"}, Body: []byte(`{"input":"PRIVATE","metadata":{"session_id":"same"}}`)}
	r := e.Before(req)
	if r.Terminate {
		t.Fatal(string(r.ResponseBody))
	}
	stream := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: "first", SourceFormat: "openai-response", Body: []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream\",\"output\":[]}}\n\n")})
	if len(stream.Body) == 0 {
		t.Fatal("stream ID not recorded")
	}
	e.Complete("first")
	req.RequestID = "second"
	req.Body = []byte(`{"input":"PRIVATE","previous_response_id":"resp_stream"}`)
	next := e.Before(req)
	if next.Terminate {
		t.Fatal(string(next.ResponseBody))
	}
	var a, b map[string]any
	json.Unmarshal(r.Body, &a)
	json.Unmarshal(next.Body, &b)
	if a["input"] != b["input"] {
		t.Fatal("stream continuation changed mapping")
	}
	e.Complete("second")
}
func TestConcurrentStreamCompleteAndReconfigure(t *testing.T) {
	e := configured(t, privateRule)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		id := fmt.Sprintf("concurrent-%d", i)
		marker := streamRequest(t, e, id)
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: id, SourceFormat: "openai", Body: chatChunk(strings.Repeat(marker, 2), false)})
			}
		}()
		go func() { defer wg.Done(); e.Complete(id); e.Complete(id) }()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := e.Configure([]byte(privateRule)); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	if e.Stats().Inflight != 0 {
		t.Fatal(e.Stats())
	}
}
