package plugin

import (
	"encoding/json"
	"errors"

	"github.com/kkkykin/cpa-vibeguard/internal/document"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const MaxRPCBytes = 64 << 20
const Version = "0.1.0-dev"

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
	result, err := e.dispatch(method, raw)
	if err != nil {
		return RPCError(err.Error())
	}
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
