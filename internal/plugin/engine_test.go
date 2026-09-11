package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func configured(t *testing.T, raw string) *Engine {
	t.Helper()
	e := New()
	if err := e.Configure([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestStickyBypassBeforeBodyAndReconfigure(t *testing.T) {
	e := configured(t, "bypass: {headers: [{name: X-Skip, value: 'true'}]}\nlimits: {request_bytes: 1}")
	req := pluginapi.RequestInterceptRequest{RequestID: "a", SourceFormat: "unknown", Headers: http.Header{"x-skip": {"true"}}, Body: []byte(`invalid json [[redacted:v1:0123456789abcdef0123456789abcdef]]`)}
	if r := e.Before(req); r.Terminate || len(r.Body) != 0 {
		t.Fatal(r)
	}
	if err := e.Configure([]byte("on_unknown_api: reject")); err != nil {
		t.Fatal(err)
	}
	req.Headers = nil
	for _, r := range []pluginapi.RequestInterceptResponse{e.Before(req), e.After(req)} {
		if r.Terminate || len(r.Body) != 0 {
			t.Fatal(r)
		}
	}
	if r := e.Response(pluginapi.ResponseInterceptRequest{RequestID: "a", Body: req.Body}); len(r.Body) != 0 {
		t.Fatal(r)
	}
	for i := 0; i < 3; i++ {
		if r := e.Stream(pluginapi.StreamChunkInterceptRequest{RequestID: "a", ChunkIndex: i, Body: req.Body}); r.DropChunk || len(r.Body) != 0 {
			t.Fatal(r)
		}
	}
	if s := e.Stats(); s.HeaderBypass != 1 || s.Inflight != 1 || s.UnknownPassthrough != 0 {
		t.Fatal(s)
	}
	e.Complete("a")
	e.Complete("a")
	if e.Stats().Inflight != 0 {
		t.Fatal("leaked bypass state")
	}
}

func TestUnknownPolicyAndFailedReconfigure(t *testing.T) {
	e := configured(t, "on_unknown_api: passthrough")
	if r := e.Before(pluginapi.RequestInterceptRequest{RequestID: "unknown", SourceFormat: "future"}); r.Terminate {
		t.Fatal(r)
	}
	old := e.current
	if err := e.Configure([]byte("api: openai")); err == nil {
		t.Fatal("bad reconfigure accepted")
	}
	if e.current != old || e.Stats().Inflight != 1 {
		t.Fatal("failed reconfigure changed snapshot or state")
	}
	if err := e.Configure(nil); err != nil {
		t.Fatal(err)
	}
	if r := e.Before(pluginapi.RequestInterceptRequest{RequestID: "reject", SourceFormat: "future"}); !r.Terminate || r.StatusCode != 400 {
		t.Fatal(r)
	}
	if r := e.Before(pluginapi.RequestInterceptRequest{RequestID: "unknown"}); r.Terminate {
		t.Fatal("old decision changed")
	}
	if s := e.Stats(); s.UnknownPassthrough != 1 || s.Rejected != 1 {
		t.Fatal(s)
	}
}

func TestCapacityCompletionAndQuiesce(t *testing.T) {
	e := configured(t, "on_unknown_api: passthrough\nlimits: {max_inflight: 1}")
	if r := e.Before(pluginapi.RequestInterceptRequest{RequestID: "a"}); r.Terminate {
		t.Fatal(r)
	}
	if r := e.Before(pluginapi.RequestInterceptRequest{RequestID: "b"}); !r.Terminate || r.StatusCode != 429 {
		t.Fatal(r)
	}
	e.Complete("b")
	e.Complete("a")
	e.Complete("a")
	if r := e.Before(pluginapi.RequestInterceptRequest{RequestID: "c"}); r.Terminate {
		t.Fatal(r)
	}
	e.Quiesce()
	if r := e.After(pluginapi.RequestInterceptRequest{RequestID: "c"}); r.Terminate {
		t.Fatal(r)
	}
	if r := e.Before(pluginapi.RequestInterceptRequest{RequestID: "d"}); !r.Terminate || r.StatusCode != 503 {
		t.Fatal(r)
	}
	e.Complete("c")
	if e.Stats().Inflight != 0 {
		t.Fatal("inflight not drained")
	}
	e.Close()
	e.Close()
	if err := e.Configure(nil); err == nil {
		t.Fatal("closed engine reconfigured")
	}
}

func TestConcurrentLifecycle(t *testing.T) {
	e := configured(t, "on_unknown_api: passthrough")
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Go(func() {
			id := fmt.Sprint(i)
			r := pluginapi.RequestInterceptRequest{RequestID: id}
			e.Before(r)
			e.After(r)
			e.Complete(id)
			e.Complete(id)
		})
	}
	wg.Wait()
	if e.Stats().Inflight != 0 {
		t.Fatal("leaked states")
	}
}

func TestRPCRegistrationAndTerminationEnvelope(t *testing.T) {
	e := New()
	defer e.Close()
	call := func(method string, v any) pluginabi.Envelope {
		t.Helper()
		b, _ := json.Marshal(v)
		var env pluginabi.Envelope
		if err := json.Unmarshal(e.Handle(method, b), &env); err != nil {
			t.Fatal(err)
		}
		return env
	}
	if env := call(pluginabi.MethodPluginRegister, lifecycleRequest{SchemaVersion: 5}); env.OK {
		t.Fatal("accepted old schema")
	}
	env := call(pluginabi.MethodPluginRegister, lifecycleRequest{SchemaVersion: 6})
	var reg registration
	if !env.OK || json.Unmarshal(env.Result, &reg) != nil || reg.Metadata.Name != "cpa-vibeguard" || reg.SchemaVersion != 6 || reg.Capabilities != (capabilities{true, true, true, true}) {
		t.Fatalf("%+v %+v", env, reg)
	}
	if reg.Metadata.Author == "" || reg.Metadata.GitHubRepository == "" {
		t.Fatal("host requires author and repository metadata")
	}
	env = call(pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{RequestID: "deny"})
	var r pluginapi.RequestInterceptResponse
	if !env.OK || json.Unmarshal(env.Result, &r) != nil || !r.Terminate {
		t.Fatal("business rejection must be a successful RPC")
	}
	for _, method := range []string{pluginabi.MethodRequestComplete, pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown} {
		if env := call(method, map[string]string{"RequestID": "deny"}); !env.OK {
			t.Fatal(env)
		}
	}
	if env := call("does.not.exist", nil); env.OK {
		t.Fatal(env)
	}
}
