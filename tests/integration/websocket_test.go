//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

func responseEvents(marker, id string) []map[string]any {
	messageID, toolID, callID := "msg_"+id, "fc_"+id, "call_"+id
	args := `{"q":"` + marker + `"}`
	message := map[string]any{"id": messageID, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": marker}}}
	tool := map[string]any{"id": toolID, "type": "function_call", "name": "lookup", "call_id": callID, "arguments": args, "status": "completed"}
	return []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "output": []any{}}},
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": messageID, "type": "message", "role": "assistant", "content": []any{}}},
		{"type": "response.output_text.delta", "output_index": 0, "item_id": messageID, "content_index": 0, "delta": marker[:20]},
		{"type": "response.output_text.delta", "output_index": 0, "item_id": messageID, "content_index": 0, "delta": marker[20:]},
		{"type": "response.output_text.done", "output_index": 0, "item_id": messageID, "content_index": 0, "text": marker},
		{"type": "response.output_item.done", "output_index": 0, "item": message},
		{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"id": toolID, "type": "function_call", "name": "lookup", "call_id": callID, "arguments": ""}},
		{"type": "response.function_call_arguments.delta", "output_index": 1, "item_id": toolID, "delta": args[:25]},
		{"type": "response.function_call_arguments.delta", "output_index": 1, "item_id": toolID, "delta": args[25:]},
		{"type": "response.function_call_arguments.done", "output_index": 1, "item_id": toolID, "arguments": args},
		{"type": "response.output_item.done", "output_index": 1, "item": tool},
		{"type": "response.completed", "response": map[string]any{"id": id, "object": "response", "status": "completed", "output": []any{message, tool}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 3, "total_tokens": 4}}},
	}
}
func websocketProviders(base string, native bool) string {
	return fmt.Sprintf(`codex-api-key:
  - api-key: fixture-ws
    base-url: %q
    websockets: %v
    models: [{name: gpt-5-codex, alias: fixture-ws, is-compat: true}]
`, base, native)
}

const websocketSession = "\n      session:\n        profiles: [{namespace: websocket, sources: [{metadata: /execution_session_id}]}]"

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, res, err := websocket.DefaultDialer.Dial(strings.Replace(url, "http://", "ws://", 1), http.Header{"Authorization": {"Bearer fixture-client"}})
	if err != nil {
		if res != nil {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			t.Fatalf("websocket dial: %v %s", err, b)
		}
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	return c
}
func wsTurn(previous string) map[string]any {
	input := []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "PRIVATE"}}}}
	if previous != "" {
		input = append([]any{map[string]any{"type": "function_call_output", "call_id": "call_" + previous, "output": "PRIVATE"}}, input...)
	}
	m := map[string]any{"type": "response.create", "model": "fixture-ws", "input": input, "tools": []any{map[string]any{"type": "function", "name": "lookup", "description": "PRIVATE", "parameters": map[string]any{"type": "object"}}}}
	if previous != "" {
		m["previous_response_id"] = previous
	}
	return m
}
func readWSTurn(t *testing.T, c *websocket.Conn) (string, []map[string]any) {
	t.Helper()
	var events []map[string]any
	for i := 0; i < 64; i++ {
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m["type"] == "error" {
			t.Fatalf("websocket request error: %s", raw)
		}
		if bytes.Contains(raw, []byte(token.Prefix)) {
			t.Fatalf("websocket leaked marker: %s", raw)
		}
		events = append(events, m)
		if m["type"] == "response.completed" {
			r := m["response"].(map[string]any)
			return r["id"].(string), events
		}
	}
	t.Fatal("websocket completion missing")
	return "", nil
}

func TestHostResponsesWebSocketNativeAndFallback(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			var turns, connections, httpCalls atomic.Int64
			captured := make(chan string, 16)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					connections.Add(1)
					upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
					c, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer c.Close()
					for {
						_, raw, err := c.ReadMessage()
						if err != nil {
							return
						}
						marker := redactedUpstream(t, "codex", raw)
						captured <- marker
						id := fmt.Sprintf("resp_ws_%d", turns.Add(1))
						for _, event := range responseEvents(marker, id) {
							if err := c.WriteJSON(event); err != nil {
								return
							}
						}
					}
				}
				httpCalls.Add(1)
				raw, _ := io.ReadAll(r.Body)
				marker := redactedUpstream(t, "codex", raw)
				captured <- marker
				id := fmt.Sprintf("resp_ws_%d", turns.Add(1))
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range responseEvents(marker, id) {
					writeSSE(w, event)
				}
			}))
			defer upstream.Close()
			h, probe := startProbedHost(t, hostSettings{Providers: websocketProviders(upstream.URL, native), PluginConfig: hostRule + websocketSession}, false)
			var connectionMarkers []string
			for _, path := range []string{"/v1/responses", "/backend-api/codex/responses"} {
				c := dialWS(t, h.url+path)
				previous, firstMarker := "", ""
				for turn := 0; turn < 2; turn++ {
					start := probe.mark(t)
					if err := c.WriteJSON(wsTurn(previous)); err != nil {
						t.Fatal(err)
					}
					var events []map[string]any
					previous, events = readWSTurn(t, c)
					assertStreamText(t, "openai-response", events)
					var args strings.Builder
					var text strings.Builder
					for _, event := range events {
						if event["type"] == "response.output_text.delta" {
							text.WriteString(event["delta"].(string))
						}
						if event["type"] == "response.function_call_arguments.delta" {
							args.WriteString(event["delta"].(string))
						}
					}
					var tool map[string]string
					if text.String() != "PRIVATE" || json.Unmarshal([]byte(args.String()), &tool) != nil || tool["q"] != "PRIVATE" {
						t.Fatalf("WebSocket text/tool restoration: %s %s", text.String(), args.String())
					}
					marker := <-captured
					if turn == 0 {
						firstMarker = marker
					} else if marker != firstMarker {
						t.Fatal("execution_session_id did not preserve multi-turn markers")
					}
					probe.verify(t, fmt.Sprintf("websocket/%v%s/turn-%d", native, path, turn), start, "openai-response")
				}
				connectionMarkers = append(connectionMarkers, firstMarker)
				c.Close()
				// CPA rejects cross-connection references before entering the
				// plugin. This is a host boundary, not a plugin business error.
				other := dialWS(t, h.url+path)
				start := probe.mark(t)
				before := turns.Load()
				if err := other.WriteJSON(wsTurn(previous)); err != nil {
					t.Fatal(err)
				}
				_, raw, err := other.ReadMessage()
				other.Close()
				if err != nil || !bytes.Contains(raw, []byte("previous_response_not_found")) || turns.Load() != before {
					t.Fatalf("cross-connection continuation: %v %s", err, raw)
				}
				probe.noRequest(t, start)
			}
			if connectionMarkers[0] == connectionMarkers[1] {
				t.Fatal("separate websocket sessions shared markers")
			}
			if native {
				if connections.Load() != 2 || httpCalls.Load() != 0 {
					t.Fatalf("native transport: ws=%d http=%d", connections.Load(), httpCalls.Load())
				}
			} else if connections.Load() != 0 || httpCalls.Load() != 4 {
				t.Fatalf("fallback transport: ws=%d http=%d", connections.Load(), httpCalls.Load())
			}
		})
	}
}

func TestHostWebSocketDisconnectReleasesRequest(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			var pause atomic.Bool
			pause.Store(true)
			stop := make(chan struct{})
			resume := make(chan struct{})
			canceled := make(chan struct{}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
					c, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer c.Close()
					for {
						_, raw, err := c.ReadMessage()
						if err != nil {
							return
						}
						marker := redactedUpstream(t, "codex", raw)
						events := responseEvents(marker, "resp_cancel")
						if pause.Swap(false) {
							for _, event := range events[:3] {
								c.WriteJSON(event)
							}
							c.SetReadDeadline(time.Now().Add(10 * time.Second))
							_, _, _ = c.ReadMessage()
							canceled <- struct{}{}
							return
						}
						for _, event := range events {
							if c.WriteJSON(event) != nil {
								return
							}
						}
					}
				}
				raw, _ := io.ReadAll(r.Body)
				marker := redactedUpstream(t, "codex", raw)
				events := responseEvents(marker, "resp_cancel")
				w.Header().Set("Content-Type", "text/event-stream")
				if pause.Swap(false) {
					for _, event := range events[:3] {
						writeSSE(w, event)
					}
					select {
					case <-r.Context().Done():
						canceled <- struct{}{}
						return
					case <-stop:
						return
					case <-resume:
					}
					// HTTP fallback has no concurrent downstream reader while
					// forwarding. Force writes so CPA can observe the disconnect;
					// without data/keepalives/completion the lease stays active.
					ticker := time.NewTicker(20 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-r.Context().Done():
							canceled <- struct{}{}
							return
						case <-stop:
							return
						case <-ticker.C:
							writeSSE(w, map[string]any{"type": "response.in_progress", "response": map[string]any{"id": "resp_cancel", "status": "in_progress"}})
						}
					}
				}
				for _, event := range events {
					writeSSE(w, event)
				}
			}))
			defer upstream.Close()
			defer close(stop)
			h := startHostConfigured(t, hostSettings{Providers: websocketProviders(upstream.URL, native), PluginConfig: hostRule + websocketSession + "\n      limits: {max_inflight: 1}"})
			c := dialWS(t, h.url+"/v1/responses")
			if err := c.WriteJSON(wsTurn("")); err != nil {
				t.Fatal(err)
			}
			for {
				_, raw, err := c.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(raw, []byte(token.Prefix)) {
					t.Fatal("incomplete marker escaped before disconnect")
				}
				var event map[string]any
				json.Unmarshal(raw, &event)
				if event["type"] == "response.output_text.delta" {
					if event["delta"] != "" {
						t.Fatalf("tail was delivered: %s", raw)
					}
					break
				}
			}
			c.Close()
			if !native {
				// The hijacked WebSocket request context has not been canceled
				// just because the client closed its socket. Keep active leases.
				status, raw := post(t, h.url+"/v1/responses", `{"model":"fixture-ws","input":"PRIVATE"}`, false)
				if status != 429 || !bytes.Contains(raw, []byte("inflight_limit")) {
					t.Fatalf("silent fallback unexpectedly completed: %d %s", status, raw)
				}
			}
			close(resume)
			deadline := time.Now().Add(5 * time.Second)
			for {
				status, raw := post(t, h.url+"/v1/responses", `{"model":"fixture-ws","input":"PRIVATE"}`, false)
				if status == 200 {
					if !bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte(token.Prefix)) {
						t.Fatal(string(raw))
					}
					break
				}
				if status != 429 || time.Now().After(deadline) {
					t.Fatalf("disconnected request retained lease/capacity: %d %s", status, raw)
				}
				time.Sleep(20 * time.Millisecond)
			}
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("host did not cancel the disconnected upstream execution")
			}
		})
	}
}

func partialResponseEvents(marker string, split int) []map[string]any {
	id := fmt.Sprintf("resp_partial_%d", split)
	itemID := "msg_" + id
	part := map[string]any{"type": "output_text", "text": marker[:split]}
	item := map[string]any{"id": itemID, "type": "message", "role": "assistant", "status": "completed", "content": []any{part}}
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "output": []any{}}},
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": itemID, "type": "message", "role": "assistant", "content": []any{}}},
		{"type": "response.output_text.delta", "output_index": 0, "item_id": itemID, "content_index": 0, "delta": marker[:split]},
		{"type": "response.output_text.done", "output_index": 0, "item_id": itemID, "content_index": 0, "text": marker[:split]},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{"id": id, "object": "response", "status": "completed", "output": []any{item}}},
	}
	for i, event := range events {
		event["sequence_number"] = i
	}
	return events
}

func TestHostWebSocketNormalPartialTail(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprint(native), func(t *testing.T) {
			var split atomic.Int64
			captured := make(chan string, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
					c, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer c.Close()
					_, raw, err := c.ReadMessage()
					if err != nil {
						t.Error(err)
						return
					}
					marker := redactedUpstream(t, "codex", raw)
					captured <- marker
					for _, event := range partialResponseEvents(marker, int(split.Load())) {
						if err := c.WriteJSON(event); err != nil {
							return
						}
					}
					c.SetReadDeadline(time.Now().Add(10 * time.Second))
					_, _, _ = c.ReadMessage()
					return
				}
				raw, _ := io.ReadAll(r.Body)
				marker := redactedUpstream(t, "codex", raw)
				captured <- marker
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range partialResponseEvents(marker, int(split.Load())) {
					writeSSE(w, event)
				}
			}))
			defer upstream.Close()
			h := startHostConfigured(t, hostSettings{Providers: websocketProviders(upstream.URL, native), PluginConfig: hostRule + websocketSession})
			for _, position := range []int{1, 13, 30, 47} {
				split.Store(int64(position))
				c := dialWS(t, h.url+"/v1/responses")
				if err := c.WriteJSON(wsTurn("")); err != nil {
					t.Fatal(err)
				}
				var text strings.Builder
				done := false
				for i := 0; ; i++ {
					_, raw, err := c.ReadMessage()
					if err != nil {
						t.Fatal(err)
					}
					var event map[string]any
					if err := json.Unmarshal(raw, &event); err != nil || event["type"] == "error" || event["sequence_number"] != float64(i) {
						t.Fatalf("partial tail event %d: %v %s", i, err, raw)
					}
					switch event["type"] {
					case "response.output_text.delta":
						if done {
							t.Fatal("tail delta appeared after field completion")
						}
						text.WriteString(event["delta"].(string))
					case "response.output_text.done":
						done = true
					}
					if event["type"] == "response.completed" {
						if i != 6 {
							t.Fatal("normal partial tail did not produce a separate delta")
						}
						break
					}
				}
				c.Close()
				if marker := <-captured; !done || text.String() != marker[:position] {
					t.Fatalf("normal partial tail at %d: %q", position, text.String())
				}
			}
		})
	}
}
