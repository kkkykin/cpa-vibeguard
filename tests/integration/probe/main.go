//go:build integration

// This test-only C ABI plugin records hook metadata and can inject RPC faults.
// It never records body text, headers, identity values, or session identifiers.
package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef struct { uint32_t abi_version; void* ctx; void* call; void* free_buffer; } cliproxy_host_api;
typedef int (*call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*free_fn)(void*, size_t);
typedef void (*shutdown_fn)(void);
typedef struct { uint32_t abi_version; call_fn call; free_fn free_buffer; shutdown_fn shutdown; } cliproxy_plugin_api;
extern int probeCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void probeFree(void*, size_t);
extern void probeShutdown(void);
*/
import "C"

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var logMu sync.Mutex

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil || host.abi_version != 1 {
		return 1
	}
	api.abi_version = 1
	api.call = C.call_fn(C.probeCall)
	api.free_buffer = C.free_fn(C.probeFree)
	api.shutdown = C.shutdown_fn(C.probeShutdown)
	return 0
}

type hookRequest struct {
	RequestID, SourceFormat, ToFormat, Outcome string
	Body, OriginalRequest                      []byte
	HistoryChunks                              [][]byte
	Headers                                    http.Header
	Metadata                                   map[string]any
	ChunkIndex                                 int
}

func record(method string, r hookRequest) {
	shape := "empty"
	raw := bytes.TrimSpace(r.Body)
	var keys []string
	if len(raw) > 0 {
		switch {
		case bytes.HasPrefix(raw, []byte("data:")), bytes.HasPrefix(raw, []byte("event:")), bytes.HasPrefix(raw, []byte(":")):
			shape = "sse"
		case json.Valid(raw) && raw[0] == '{':
			shape = "json-object"
			var m map[string]any
			json.Unmarshal(raw, &m)
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
		case json.Valid(raw) && raw[0] == '[':
			shape = "json-array"
		case bytes.HasPrefix(raw, []byte("--")):
			shape = "multipart"
		default:
			shape = "fragment"
		}
	}
	path, _ := r.Metadata["request_path"].(string)
	caller, _ := r.Metadata["caller_scope"].(string)
	execution, _ := r.Metadata["execution_session_id"].(string)
	item := map[string]any{"method": method, "request_id": r.RequestID, "source_format": r.SourceFormat, "to_format": r.ToFormat, "path": path, "shape": shape, "keys": keys, "chunk_index": r.ChunkIndex, "original_bytes": len(r.OriginalRequest), "history_chunks": len(r.HistoryChunks), "outcome": r.Outcome, "caller_present": caller != "", "execution_session_present": execution != ""}
	line, _ := json.Marshal(item)
	line = append(line, '\n')
	logMu.Lock()
	defer logMu.Unlock()
	if file, err := os.OpenFile(os.Getenv("VIBEGUARD_PROBE_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
		file.Write(line)
		file.Close()
	}
}

func dispatch(method string, raw []byte) pluginabi.Envelope {
	var result any = struct{}{}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		result = map[string]any{"schema_version": 6, "metadata": pluginapi.Metadata{Name: "cpa-vibeguard-probe", Version: "0.0.0-test", Author: "integration fixture", GitHubRepository: "https://github.com/kkkykin/cpa-vibeguard"}, "capabilities": map[string]bool{"request_interceptor": true, "response_interceptor": true, "response_stream_interceptor": true, "request_lifecycle_plugin": true}}
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
	default:
		var r hookRequest
		if err := json.Unmarshal(raw, &r); err != nil {
			return pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: "fixture_decode", Message: "fixture_decode"}}
		}
		record(method, r)
		if strings.HasPrefix(method, "request.intercept_") {
			switch r.Headers.Get("X-Fixture-Mode") {
			case "rpc-error":
				return pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: "fixture_rpc_error", Message: "fixture_rpc_error"}}
			case "reject":
				result = pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: 422, ResponseHeaders: http.Header{"Content-Type": {"application/json"}}, ResponseBody: []byte(`{"error":{"code":"fixture_business_rejection"}}`)}
			}
		}
	}
	encoded, _ := json.Marshal(result)
	return pluginabi.Envelope{OK: true, Result: encoded}
}

//export probeCall
func probeCall(method *C.char, request *C.uint8_t, size C.size_t, response *C.cliproxy_buffer) C.int {
	if response == nil || method == nil || size > 64<<20 {
		return 1
	}
	response.ptr, response.len = nil, 0
	encoded, _ := json.Marshal(dispatch(C.GoString(method), C.GoBytes(unsafe.Pointer(request), C.int(size))))
	response.ptr = C.CBytes(encoded)
	response.len = C.size_t(len(encoded))
	return 0
}

//export probeFree
func probeFree(ptr unsafe.Pointer, _ C.size_t) { C.free(ptr) }

//export probeShutdown
func probeShutdown() {}
