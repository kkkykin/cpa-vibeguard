package plugin

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/kkkykin/cpa-vibeguard/internal/document"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const MaxRPCBytes = 64 << 20

// Version is set by release builds with -ldflags -X.
var Version = "0.1.0-dev"

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}
type capabilities struct {
	Request   bool `json:"request_interceptor"`
	Response  bool `json:"response_interceptor"`
	Stream    bool `json:"response_stream_interceptor"`
	Lifecycle bool `json:"request_lifecycle_plugin"`
}
type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
	// CPA ignores this diagnostic, but returning the existing capabilities
	// keeps its active hooks installed when a candidate configuration fails.
	ConfigurationError string `json:"configuration_error,omitempty"`
}

func Registration() registration {
	return registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: pluginapi.Metadata{Name: "cpa-vibeguard", Version: Version, Author: "cpa-vibeguard contributors", GitHubRepository: "https://github.com/kkkykin/cpa-vibeguard"}, Capabilities: capabilities{true, true, true, true}}
}
func RPCError(code string) []byte {
	b, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: code}})
	return b
}
func decodeRPC(raw []byte, dst any) error {
	if len(raw) > MaxRPCBytes {
		return errors.New("rpc_size_limit")
	}
	if _, err := document.JSON(raw); err != nil {
		return errors.New("invalid_rpc_request")
	}
	if json.Unmarshal(raw, dst) != nil {
		return errors.New("invalid_rpc_request")
	}
	return nil
}
func (e *Engine) Handle(method string, raw []byte) []byte {
	if len(raw) > MaxRPCBytes {
		return e.HandleOversizedRPC(method)
	}
	result, err := e.dispatch(method, raw)
	if err != nil {
		return RPCError(err.Error())
	}
	return rpcResult(result)
}

// HandleOversizedRPC can be called before copying a native request into Go.
// CPA skips RPC failures, so request hooks must return a successful business
// rejection even when the envelope is too large to inspect its ID or headers.
func (e *Engine) HandleOversizedRPC(method string) []byte {
	switch method {
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		e.metrics.rejected.Add(1)
		return rpcResult(termination(http.StatusRequestEntityTooLarge, "rpc_size_limit"))
	default:
		return RPCError("rpc_size_limit")
	}
}

func rpcResult(result any) []byte {
	body, err := json.Marshal(result)
	if err != nil {
		return RPCError("rpc_encode_error")
	}
	b, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: body})
	return b
}
func (e *Engine) dispatch(method string, raw []byte) (any, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if err := decodeRPC(raw, &req); err != nil {
			return nil, err
		}
		if req.SchemaVersion < 6 {
			return nil, errors.New("host_schema_6_required")
		}
		if err := e.Configure(req.ConfigYAML); err != nil {
			if method == pluginabi.MethodPluginReconfigure && e.retainConfiguration() {
				// CPA removes a plugin's active hooks on an RPC reconfigure
				// error. Report validation failure without unregistering the
				// previous working configuration. Configure errors contain
				// only error codes, field locations, source paths and rule IDs.
				log.Printf("cpa-vibeguard: reconfigure_rejected; previous configuration retained: %q", err.Error())
				reg := Registration()
				reg.ConfigurationError = err.Error()
				return reg, nil
			}
			return nil, err
		}
		return Registration(), nil
	case pluginabi.MethodPluginQuiesce:
		e.Quiesce()
		return struct{}{}, nil
	case pluginabi.MethodPluginShutdown:
		e.Close()
		return struct{}{}, nil
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		var req pluginapi.RequestInterceptRequest
		if err := decodeRPC(raw, &req); err != nil {
			return nil, err
		}
		if method == pluginabi.MethodRequestInterceptBefore {
			return e.Before(req), nil
		}
		return e.After(req), nil
	case pluginabi.MethodResponseInterceptAfter:
		var req pluginapi.ResponseInterceptRequest
		if err := decodeRPC(raw, &req); err != nil {
			return nil, err
		}
		return e.Response(req), nil
	case pluginabi.MethodResponseInterceptStreamChunk:
		var req pluginapi.StreamChunkInterceptRequest
		if err := decodeRPC(raw, &req); err != nil {
			return nil, err
		}
		return e.Stream(req), nil
	case pluginabi.MethodRequestComplete:
		var req pluginapi.RequestCompletion
		if err := decodeRPC(raw, &req); err != nil {
			return nil, err
		}
		e.Complete(req.RequestID)
		return struct{}{}, nil
	default:
		return nil, errors.New("unknown_rpc_method")
	}
}

func (e *Engine) retainConfiguration() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil || e.closed {
		return false
	}
	// During replacement rollback CPA can resend a previously rejected
	// candidate. Restore the working snapshot's availability in that case too.
	e.quiesced = false
	return true
}
