package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

func TestRuleConfigurationIsAtomic(t *testing.T) {
	e := configured(t, "on_unknown_api: passthrough")
	old := e.current
	if err := e.Configure([]byte("disabled_rules: [unknown-rule]")); err == nil {
		t.Fatal("unknown disabled rule accepted")
	}
	if e.current != old {
		t.Fatal("invalid rules replaced the snapshot")
	}
	if err := e.Configure([]byte("rule_sources: [{inline: {rules: [{id: bad, regex: '('}]}}]")); err == nil {
		t.Fatal("invalid regex accepted")
	}
	if e.current != old {
		t.Fatal("invalid regex replaced the snapshot")
	}
	if err := e.Configure([]byte("disabled_rules: [generic-api-key]")); err != nil {
		t.Fatal(err)
	}
	if e.current.generation != old.generation+1 {
		t.Fatal("snapshot generation not advanced")
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

func TestRPCRejectedReconfigureKeepsHooksAndProtection(t *testing.T) {
	e := New()
	defer e.Close()
	call := func(method, config string) (pluginabi.Envelope, registration) {
		t.Helper()
		raw, _ := json.Marshal(lifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte(config)})
		var env pluginabi.Envelope
		var reg registration
		if err := json.Unmarshal(e.Handle(method, raw), &env); err != nil {
			t.Fatal(err)
		}
		if env.OK {
			if err := json.Unmarshal(env.Result, &reg); err != nil {
				t.Fatal(err)
			}
		}
		return env, reg
	}
	badConfig := "rule_sources: [{inline: {rules: [{id: invalid, regex: 'PRIVATE('}]}}]"
	if env, _ := call(pluginabi.MethodPluginRegister, badConfig); env.OK {
		t.Fatal("initial invalid configuration must fail registration")
	}
	if env, _ := call(pluginabi.MethodPluginRegister, privateRule); !env.OK {
		t.Fatal(env)
	}
	for _, invalid := range []string{badConfig, "limits: {request_bytes: PRIVATE}", "unexpected: PRIVATE"} {
		e.Quiesce() // A failed replacement can reconfigure the old instance.
		env, reg := call(pluginabi.MethodPluginReconfigure, invalid)
		if !env.OK || reg.ConfigurationError == "" || reg.Capabilities != Registration().Capabilities {
			t.Fatalf("rejected update must preserve CPA's hook registration: %+v %+v", env, reg)
		}
		if strings.Contains(reg.ConfigurationError, "PRIVATE") {
			t.Fatal("configuration diagnostic exposed a value")
		}
		r := e.Before(pluginapi.RequestInterceptRequest{RequestID: "after-update", SourceFormat: "openai", Body: []byte(`{"messages":[{"role":"user","content":"PRIVATE"}]}`)})
		if r.Terminate || len(r.Body) == 0 || bytes.Contains(r.Body, []byte("PRIVATE")) {
			t.Fatalf("rejected update removed protection: %+v", r)
		}
		e.Complete("after-update")
	}
	e.Quiesce()
	if env, reg := call(pluginabi.MethodPluginReconfigure, privateRule); !env.OK || reg.ConfigurationError != "" {
		t.Fatal("valid reconfigure did not resume the quiesced instance")
	}
	if r := e.Before(pluginapi.RequestInterceptRequest{RequestID: "resumed", SourceFormat: "openai", Body: []byte(`{"messages":[{"role":"user","content":"PRIVATE"}]}`)}); r.Terminate {
		t.Fatal(r)
	}
	e.Close()
	if env, _ := call(pluginabi.MethodPluginReconfigure, privateRule); env.OK {
		t.Fatal("closed instance resumed")
	}
}
