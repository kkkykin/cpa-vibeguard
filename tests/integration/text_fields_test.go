//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kkkykin/cpa-vibeguard/internal/token"
)

func TestHostPredictionRedaction(t *testing.T) {
	// Synthetic token recognized by the default Gitleaks rules.
	const secret = "ghp_0123456789abcdef0123456789ABCDEFGHij"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages   []struct{ Content string }
			Prediction struct {
				Type    string
				Content any
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 1 {
			t.Errorf("invalid upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		marker := body.Messages[0].Content
		if !token.Valid(marker) || body.Prediction.Type != "content" {
			t.Error("message was not redacted or prediction type changed")
		}
		switch content := body.Prediction.Content.(type) {
		case string:
			if content != marker {
				t.Errorf("prediction text escaped redaction: %q", content)
			}
		case []any:
			if len(content) != 2 {
				t.Errorf("prediction part count changed: %d", len(content))
			}
			for _, v := range content {
				part, ok := v.(map[string]any)
				if !ok || part["type"] != "text" || part["text"] != marker {
					t.Errorf("prediction part escaped redaction or changed shape: %+v", v)
				}
			}
		default:
			t.Errorf("unexpected prediction content: %+v", content)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": marker}, "finish_reason": "stop",
		}}})
	}))
	defer upstream.Close()
	h := startHost(t, upstream.URL, "")
	for _, tc := range []struct {
		name    string
		content any
	}{
		{"string", secret},
		{"parts", []any{map[string]any{"type": "text", "text": secret}, map[string]any{"type": "text", "text": secret}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := json.Marshal(map[string]any{
				"model": "fixture-chat", "messages": []any{map[string]any{"role": "user", "content": secret}},
				"prediction": map[string]any{"type": "content", "content": tc.content},
			})
			if err != nil {
				t.Fatal(err)
			}
			status, raw := post(t, h.url+"/v1/chat/completions", string(request), false)
			var response struct {
				Choices []struct{ Message struct{ Content string } }
			}
			if status != http.StatusOK || json.Unmarshal(raw, &response) != nil || len(response.Choices) != 1 || response.Choices[0].Message.Content != secret {
				t.Fatalf("prediction round trip: HTTP %d, %s", status, raw)
			}
		})
	}
}
