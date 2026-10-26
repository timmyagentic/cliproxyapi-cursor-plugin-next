"""Load the actual shared library through CPA's schema-v1 C ABI."""
import base64
import ctypes as c
import json
from pathlib import Path
import sys
import threading


class Buffer(c.Structure):
    _fields_ = [("ptr", c.c_void_p), ("len", c.c_size_t)]


HostCall = c.CFUNCTYPE(c.c_int, c.c_void_p, c.c_char_p, c.POINTER(c.c_uint8), c.c_size_t, c.POINTER(Buffer))
Free = c.CFUNCTYPE(None, c.c_void_p, c.c_size_t)
PluginCall = c.CFUNCTYPE(c.c_int, c.c_char_p, c.POINTER(c.c_uint8), c.c_size_t, c.POINTER(Buffer))
Shutdown = c.CFUNCTYPE(None)


class Host(c.Structure):
    _fields_ = [("abi_version", c.c_uint32), ("host_ctx", c.c_void_p), ("call", HostCall), ("free_buffer", Free)]


class Plugin(c.Structure):
    _fields_ = [("abi_version", c.c_uint32), ("call", PluginCall), ("free_buffer", Free), ("shutdown", Shutdown)]


buffers = {}
streams = {}
http_requests = []
callback_lock = threading.RLock()
next_handle = 0


@HostCall
def host_callback(host_ctx, method, request, length, response):
    global next_handle
    with callback_lock:
        name = method.decode()
        req = json.loads(c.string_at(request, length))
        assert req["host_callback_id"] == "abi-quota-context"
        result = {}
        if name == "host.http.operation_open":
            next_handle += 1
            result = {"operation_id": str(next_handle)}
        elif name == "host.http.do_stream":
            http_requests.append(req)
            if req["url"].endswith("GetCurrentPeriodUsage"):
                body = {"planUsage": {"autoPercentUsed": 25, "apiPercentUsed": 50, "totalPercentUsed": 30}}
            elif req["url"].endswith("GetPlanInfo"):
                body = {"planInfo": {"planName": "Pro"}}
            elif req["url"].endswith("GetHardLimit"):
                body = {"noUsageBasedAllowed": True}
            else:
                return 1
            next_handle += 1
            stream_id = str(next_handle)
            streams[stream_id] = json.dumps(body).encode()
            result = {"status_code": 200, "headers": {"Content-Type": ["application/json"]}, "stream_id": stream_id}
        elif name == "host.http.stream_read":
            body = streams.pop(req["stream_id"], None)
            result = {"payload": base64.b64encode(body).decode()} if body is not None else {"done": True}
        elif name == "host.http.stream_close":
            streams.pop(req["stream_id"], None)
        elif name != "host.http.cancel":
            return 1
        raw = json.dumps({"ok": True, "result": result}).encode()
        buf = c.create_string_buffer(raw)
        ptr = c.addressof(buf)
        buffers[ptr] = buf
        response.contents.ptr = ptr
        response.contents.len = len(raw)
        return 0


@Free
def free_callback(*args):
    with callback_lock:
        buffers.pop(args[0], None)


library = c.CDLL(str(Path(sys.argv[1]).resolve()))
library.cliproxy_plugin_init.argtypes = [c.POINTER(Host), c.POINTER(Plugin)]
library.cliproxy_plugin_init.restype = c.c_int
host = Host(1, None, host_callback, free_callback)
plugin = Plugin()
assert library.cliproxy_plugin_init(c.byref(host), c.byref(plugin)) == 0
assert plugin.abi_version == 1


def call(method, request):
    raw = json.dumps(request).encode()
    data = (c.c_uint8 * len(raw)).from_buffer_copy(raw)
    response = Buffer()
    assert plugin.call(method.encode(), data, len(raw), c.byref(response)) == 0
    try:
        return json.loads(c.string_at(response.ptr, response.len))
    finally:
        plugin.free_buffer(response.ptr, response.len)


try:
    registered = call("plugin.register", {})
    assert registered["ok"] and registered["result"]["capabilities"]["executor"]
    assert registered["result"]["capabilities"]["quota_provider"]
    assert registered["result"]["capabilities"]["management_api"]
    assert registered["result"]["metadata"]["Name"] == "Cursor"
    assert registered["result"]["metadata"]["Logo"].endswith("/assets/cursor.png")
    routes = call("management.register", {"BasePath": "/v0/management", "ResourceBasePath": "/v0/resource/plugins/spx-cursor"})["result"]
    assert routes["routes"][0]["Method"] == "POST"
    page = call("management.handle", {"Method": "GET", "Path": routes["resources"][0]["Path"]})["result"]
    assert page["StatusCode"] == 200
    assert b'Cursor API Key' in base64.b64decode(page["Body"])
    assert registered["result"]["metadata"]["Version"] == json.loads((Path(__file__).resolve().parents[1] / "registry.json").read_text())["plugins"][0]["version"]
    assert call("quota.identifier", {})["result"]["identifier"] == "cursor"
    assert call("quota.describe", {})["result"]["supported_providers"] == ["cursor"]
    assert call("quota.reset", {})["error"]["http_status"] == 501
    raw_auth = {"type": "cursor", "cursor_auth": {"type": "api", "key": "synthetic-abi-key"}}
    parsed = call("auth.parse", {"RawJSON": base64.b64encode(json.dumps(raw_auth).encode()).decode(), "FileName": "fixture.json"})
    assert parsed["ok"] and parsed["result"]["Auth"]["Provider"] == "cursor"
    assert call("model.static", {})["result"]["Models"] == []
    assert call("executor.count_tokens", {})["error"]["http_status"] == 501
    # CPA's canonical "openai" name must reach credential validation rather
    # than fail at the format guard, for both executor entry points.
    for method in ("executor.execute", "executor.execute_stream"):
        result = call(method, {
            "Format": "openai", "stream_id": "format-check",
            "Payload": base64.b64encode(b'{"messages":[{"role":"user","content":"hello"}]}').decode(),
        })
        assert result["error"]["http_status"] == 401, result
    oauth = {"type": "cursor", "cursor_auth": {"type": "oauth", "access": "synthetic-abi-token", "refresh": "none", "expires": 9999999999999}}
    quota = call("quota.fetch", {"host_callback_id": "abi-quota-context", "storage_json": base64.b64encode(json.dumps(oauth).encode()).decode()})
    assert quota["ok"], quota
    assert quota["result"]["subscription"]["plan"] == "Pro"
    assert quota["result"]["groups"][0]["buckets"][0]["remainingFraction"] == 0.75
    assert len(http_requests) == 3
    assert all(req["wire_profile"]["http1_only"] for req in http_requests)
    assert not buffers and not streams, "CPA callback buffers or streams leaked"
    print("C ABI smoke test passed (registration, auth parsing, quota HTTP callbacks, status mapping, buffer ownership, shutdown).")
finally:
    plugin.shutdown()
