"""Exercise the real C function table, envelopes, and buffer ownership."""
import base64
import ctypes as c
import json
import pathlib
import re
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


store_config = (pathlib.Path(__file__).parents[2] / "testdata/plugin-store.yaml").read_text()


def lifecycle(config="", schema=6):
    config = store_config + "\n" + config
    return {"schema_version": schema,
            "config_yaml": base64.b64encode(config.encode()).decode()}


call("plugin.register", lifecycle(schema=5), ok=False)
reg = call("plugin.register", lifecycle("bypass: {headers: [{name: X-Skip, present: true}]}"))
assert reg["metadata"]["Name"] == "cpa-vibeguard"
if len(sys.argv) > 2:
    assert reg["metadata"]["Version"] == sys.argv[2], reg["metadata"]
assert reg["schema_version"] == 6
assert len(reg["capabilities"]) == 4 and all(reg["capabilities"].values())
body = base64.b64encode(b"malformed [[redacted:v1:0123456789abcdef0123456789abcdef]]").decode()
request = {"RequestID": "native-1", "Headers": {"X-Skip": ["yes"]}, "Body": body}
assert not call("request.intercept_before", request)["Terminate"]
rejected_update = call("plugin.reconfigure", lifecycle("unexpected: invalid"))
assert "configuration_error" in rejected_update
assert all(rejected_update["capabilities"].values())
assert not call("request.intercept_after", request)["Terminate"]
assert not call("response.intercept_after", request)["Body"]
assert not call("response.intercept_stream_chunk", dict(request, ChunkIndex=0))["DropChunk"]
for _ in range(2):
    call("request.complete", {"RequestID": "native-1", "Outcome": "canceled"})
denied = call("request.intercept_before", {"RequestID": "native-deny"})
assert denied["Terminate"] and denied["StatusCode"] == 400
call("request.complete", {"RequestID": "native-deny", "Outcome": "rejected"})

rule_config = "rule_sources: [{inline: {extend: {useDefault: false}, rules: [{id: fixture, regex: PRIVATE}]}}]"
call("plugin.reconfigure", lifecycle(rule_config))
fixtures = json.loads((pathlib.Path(__file__).parents[2] / "testdata/protocols.json").read_text())


def normalize(value):
    if isinstance(value, dict):
        return {k: normalize(v) for k, v in value.items()}
    if isinstance(value, list):
        return [normalize(v) for v in value]
    if isinstance(value, str) and value.startswith(("{", "[")):
        try:
            return normalize(json.loads(value))
        except json.JSONDecodeError:
            pass
    return value


def replace_fields(value, marker):
    # Fixture payloads distinguish selected text (exact PRIVATE) from opaque
    # strings (PRIVATE_MEDIA, PRIVATE_ID, etc.). Preserve the latter verbatim.
    if isinstance(value, dict):
        return {k: replace_fields(v, marker) for k, v in value.items()}
    if isinstance(value, list):
        return [replace_fields(v, marker) for v in value]
    if value == "PRIVATE":
        return marker
    if isinstance(value, str) and value.startswith(("{", "[")):
        try:
            return json.dumps(replace_fields(json.loads(value), marker))
        except json.JSONDecodeError:
            pass
    return value


for fixture in fixtures:
    request = dict(fixture["request"])
    request.pop("previous_response_id", None)
    request.pop("previous_interaction_id", None)
    request_id = "native-" + fixture["name"]
    intercepted = call("request.intercept_before", {
        "RequestID": request_id, "SourceFormat": fixture["format"],
        "Body": base64.b64encode(json.dumps(request).encode()).decode()})
    assert not intercepted["Terminate"], intercepted
    masked = base64.b64decode(intercepted["Body"]).decode()
    markers = set(re.findall(r"\[\[redacted:v1:[0-9a-f]{32}\]\]", masked))
    assert len(markers) == 1, fixture["name"]
    marker = markers.pop()
    response = replace_fields(fixture["response"], marker)
    restored = call("response.intercept_after", {
        "RequestID": request_id, "SourceFormat": fixture["format"],
        "Body": base64.b64encode(json.dumps(response).encode()).decode()})
    actual = json.loads(base64.b64decode(restored["Body"]))
    assert normalize(actual) == normalize(fixture["response"]), fixture["name"]
    call("request.complete", {"RequestID": request_id, "Outcome": "succeeded"})

stream_fixtures = json.loads((pathlib.Path(__file__).parents[2] / "testdata/streams.json").read_text())
for fixture in stream_fixtures:
    request_id = "native-stream-" + fixture["name"]
    intercepted = call("request.intercept_before", {
        "RequestID": request_id, "SourceFormat": fixture["format"],
        "Body": base64.b64encode(json.dumps(fixture["request"]).encode()).decode()})
    assert not intercepted["Terminate"], intercepted
    masked = base64.b64decode(intercepted["Body"]).decode()
    marker = re.search(r"\[\[redacted:v1:[0-9a-f]{32}\]\]", masked).group()
    delivered = []
    for index, event in enumerate(fixture["events"]):
        raw = json.dumps(event).replace("@MARKER@", marker).replace("@HEAD@", marker[:20]).replace("@TAIL@", marker[20:])
        if fixture["framing"] == "sse":
            raw = "data: " + raw + "\n\n"
        result = call("response.intercept_stream_chunk", {
            "RequestID": request_id, "SourceFormat": fixture["format"],
            "ChunkIndex": index, "Body": base64.b64encode(raw.encode()).decode()})
        assert result["Body"] and not result["DropChunk"], (fixture["name"], index, result)
        output = base64.b64decode(result["Body"]).decode()
        if fixture["framing"] == "sse":
            for line in output.splitlines():
                if line.startswith("data:"):
                    json.loads(line[5:])
        else:
            json.loads(output)
        delivered.append(output)
    joined = "".join(delivered)
    assert "PRIVATE" in joined and "[[redacted:v1:" not in joined, fixture["name"]
    call("request.complete", {"RequestID": request_id, "Outcome": "succeeded"})

call("plugin.quiesce", {})
assert call("request.intercept_before", {"RequestID": "quiesced"})["StatusCode"] == 503
call("plugin.shutdown", {})
api.shutdown()
print("ABI 1 / schema 6: store configuration, lifecycle, all protocol JSON fixtures, and five stream formats passed")
