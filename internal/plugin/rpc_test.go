package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestOversizedRequestRPCIsBusinessRejection(t *testing.T) {
	e := configured(t, "on_request_error: passthrough\nbypass: {headers: [{name: X-Skip, present: true}]}")
	raw := bytes.Repeat([]byte(" "), MaxRPCBytes+1)
	for _, method := range []string{pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter} {
		t.Run(method, func(t *testing.T) {
			var envelope pluginabi.Envelope
			if err := json.Unmarshal(e.Handle(method, raw), &envelope); err != nil {
				t.Fatal(err)
			}
			var response pluginapi.RequestInterceptResponse
			if !envelope.OK || envelope.Error != nil || json.Unmarshal(envelope.Result, &response) != nil {
				t.Fatalf("host would skip RPC rejection: %+v", envelope)
			}
			if !response.Terminate || response.StatusCode != http.StatusRequestEntityTooLarge || !bytes.Contains(response.ResponseBody, []byte("rpc_size_limit")) {
				t.Fatalf("oversized request was not rejected: %+v", response)
			}
		})
	}
	if stats := e.Stats(); stats.Inflight != 0 || stats.Rejected != 2 {
		t.Fatalf("oversized envelopes must not allocate request state: %+v", stats)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(e.Handle(pluginabi.MethodResponseInterceptAfter, raw), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "rpc_size_limit" {
		t.Fatalf("response hooks cannot return request termination: %+v", envelope)
	}
}
