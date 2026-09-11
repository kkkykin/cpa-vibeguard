"""Exercise the real C function table, envelopes, and buffer ownership."""
import base64
import ctypes as c
import json
import sys


class Buffer(c.Structure):
    _fields_ = [("ptr", c.c_void_p), ("len", c.c_size_t)]


Call = c.CFUNCTYPE(c.c_int, c.c_char_p, c.c_void_p, c.c_size_t, c.POINTER(Buffer))
Free = c.CFUNCTYPE(None, c.c_void_p, c.c_size_t)
Shutdown = c.CFUNCTYPE(None)


class Host(c.Structure):
    _fields_ = [("abi", c.c_uint32), ("ctx", c.c_void_p),
                ("call", c.c_void_p), ("free", c.c_void_p)]


class API(c.Structure):
    _fields_ = [("abi", c.c_uint32), ("call", Call),
                ("free", Free), ("shutdown", Shutdown)]


lib = c.CDLL(sys.argv[1])
init = lib.cliproxy_plugin_init
init.argtypes = [c.POINTER(Host), c.POINTER(API)]
init.restype = c.c_int
api = API()
assert init(c.byref(Host(2)), c.byref(api)) != 0
assert init(c.byref(Host(1)), c.byref(api)) == 0
assert api.abi == 1


def call(method, request, ok=True):
    raw = json.dumps(request).encode()
    buf = Buffer()
    assert api.call(method.encode(), raw, len(raw), c.byref(buf)) == 0
    try:
        envelope = json.loads(c.string_at(buf.ptr, buf.len))
    finally:
        api.free(buf.ptr, buf.len)
    assert envelope["ok"] is ok, envelope
    return envelope.get("result", envelope.get("error"))


def lifecycle(config="", schema=6):
    return {"schema_version": schema,
            "config_yaml": base64.b64encode(config.encode()).decode()}


call("plugin.register", lifecycle(schema=5), ok=False)
reg = call("plugin.register", lifecycle("bypass: {headers: [{name: X-Skip, present: true}]}"))
assert reg["metadata"]["Name"] == "cpa-vibeguard"
assert reg["schema_version"] == 6
assert len(reg["capabilities"]) == 4 and all(reg["capabilities"].values())
body = base64.b64encode(b"malformed [[redacted:v1:0123456789abcdef0123456789abcdef]]").decode()
request = {"RequestID": "native-1", "Headers": {"X-Skip": ["yes"]}, "Body": body}
assert not call("request.intercept_before", request)["Terminate"]
call("plugin.reconfigure", lifecycle("unexpected: invalid"), ok=False)
assert not call("request.intercept_after", request)["Terminate"]
assert not call("response.intercept_after", request)["Body"]
assert not call("response.intercept_stream_chunk", dict(request, ChunkIndex=0))["DropChunk"]
for _ in range(2):
    call("request.complete", {"RequestID": "native-1", "Outcome": "canceled"})
denied = call("request.intercept_before", {"RequestID": "native-deny"})
assert denied["Terminate"] and denied["StatusCode"] == 400
call("request.complete", {"RequestID": "native-deny", "Outcome": "rejected"})
call("plugin.quiesce", {})
assert call("request.intercept_before", {"RequestID": "quiesced"})["StatusCode"] == 503
call("plugin.shutdown", {})
api.shutdown()
print("ABI 1 / schema 6: registration, four hooks, reconfigure, quiesce, shutdown passed")
