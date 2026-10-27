#!/usr/bin/env python3
"""Verify stock CPA retry_after_seconds through a synthetic C ABI plugin.

Uses loopback only, synthetic credentials, no provider calls, no global cooldown
configuration, and no existing service. The fixture source/build/state remain
inside a fresh state directory. Both RPC errors and stream.close are exercised.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request


FIXTURE = r'''
import "C"
import (
    "encoding/json"
    "os"
    "strings"
    "sync"
    "unsafe"
)
var mu sync.Mutex
var attempts = map[string]int{}
func main() {}
func encode(v any) []byte { b, _ := json.Marshal(v); return b }
//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
    C.store_host_api(host)
    plugin.abi_version = 1
    plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
    plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
    plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
    return 0
}
func hostCall(method string, value any) {
    cm := C.CString(method); defer C.free(unsafe.Pointer(cm))
    raw := encode(value); ptr := C.CBytes(raw); defer C.free(ptr)
    var out C.cliproxy_buffer
    C.call_host_api(cm, (*C.uint8_t)(ptr), C.size_t(len(raw)), &out)
    if out.ptr != nil { C.free_host_buffer(out.ptr, out.len) }
}
func next(model, method string) int {
    mu.Lock(); defer mu.Unlock()
    attempts[model]++
    file, err := os.OpenFile(os.Getenv("CURSOR_COOLDOWN_TRACE"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
    if err == nil { json.NewEncoder(file).Encode(map[string]any{"model":model,"method":method,"attempt":attempts[model]}); file.Close() }
    return attempts[model]
}
func detail(model string) map[string]any {
    value := map[string]any{"code":"synthetic_network_failure","message":"Synthetic transport disconnect; outcome uncertain, cannot resubmit","http_status":502}
    if strings.Contains(model,"short") { value["retry_after_seconds"] = 2 }
    return value
}
//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
    var req struct { Model, FileName string; RawJSON []byte; CallbackID string `json:"host_callback_id"`; StreamID string `json:"stream_id"` }
    _ = json.Unmarshal(C.GoBytes(unsafe.Pointer(request), C.int(requestLen)), &req)
    name := C.GoString(method)
    result := any(map[string]any{})
    envelope := map[string]any{"ok":true}
    switch name {
    case "plugin.register", "plugin.reconfigure":
        result = map[string]any{"schema_version":1,"metadata":map[string]any{"Name":"Synthetic transport cooldown fixture","Version":"0.0.0","Author":"test","GitHubRepository":"https://github.com/timmyagentic/cliproxyapi-cursor-plugin-next"},"capabilities":map[string]any{"auth_provider":true,"model_provider":true,"executor":true,"executor_model_scope":"oauth","executor_input_formats":[]string{"chat-completions"},"executor_output_formats":[]string{"chat-completions"}}}
    case "auth.identifier", "executor.identifier": result = map[string]any{"identifier":"cursor"}
    case "auth.parse": result = map[string]any{"Handled":true,"Auth":map[string]any{"ID":"synthetic-cooldown-auth","Provider":"cursor","FileName":req.FileName,"StorageJSON":req.RawJSON,"Metadata":map[string]any{"type":"cursor"}}}
    case "model.static", "model.for_auth":
        models := []any{}
        for _, suffix := range []string{"short-sync","short-stream","default-sync","default-stream"} {
            models = append(models,map[string]any{"ID":"cursor/cooldown-"+suffix,"Object":"model","OwnedBy":"cursor","Type":"cursor","Name":"cooldown-"+suffix,"ContextLength":500000,"SupportedGenerationMethods":[]string{"chat"}})
        }
        result = map[string]any{"Provider":"cursor","Models":models}
    case "executor.execute":
        if next(req.Model,name)==1 { envelope = map[string]any{"ok":false,"error":detail(req.Model)} } else {
            result = map[string]any{"Payload":encode(map[string]any{"id":"chatcmpl-synthetic","object":"chat.completion","model":req.Model,"created":1,"choices":[]any{map[string]any{"index":0,"message":map[string]any{"role":"assistant","content":"synthetic recovered"},"finish_reason":"stop"}}})}
        }
    case "executor.execute_stream":
        attempt := next(req.Model,name)
        go func() {
            fields := map[string]any{"host_callback_id":req.CallbackID,"stream_id":req.StreamID}
            if attempt==1 { fields["error"] = "Synthetic transport disconnect; outcome uncertain, cannot resubmit"; fields["error_detail"] = detail(req.Model) } else {
                chunk := map[string]any{"id":"chatcmpl-synthetic","object":"chat.completion.chunk","model":req.Model,"created":1,"choices":[]any{map[string]any{"index":0,"delta":map[string]any{"role":"assistant","content":"synthetic recovered"},"finish_reason":nil}}}
                fields["payload"] = []byte("data: "+string(encode(chunk))+"\n\n"); hostCall("host.stream.emit",fields)
                chunk["choices"] = []any{map[string]any{"index":0,"delta":map[string]any{},"finish_reason":"stop"}}
                fields["payload"] = []byte("data: "+string(encode(chunk))+"\n\n"); hostCall("host.stream.emit",fields)
                fields["payload"] = []byte("data: [DONE]\n\n"); hostCall("host.stream.emit",fields); delete(fields,"payload")
            }
            hostCall("host.stream.close",fields)
        }()
    }
    if envelope["ok"]==true { envelope["result"] = result }
    raw := encode(envelope); response.ptr=C.CBytes(raw); response.len=C.size_t(len(raw)); return 0
}
//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) { if ptr!=nil { C.free(ptr) } }
//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}
'''


def request(base, key, model=None, stream=False):
    data = None if model is None else json.dumps({"model": model, "input": "Synthetic cooldown ABI check.", "stream": stream}).encode()
    req = urllib.request.Request(base + ("/v1/models" if model is None else "/v1/responses"), data=data,
                                 headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
    try:
        response = urllib.request.urlopen(req, timeout=10)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        raw = response.read(1024 * 1024)
        row = {"status": response.status, "retry_after_header": response.headers.get("Retry-After")}
    if model is None:
        return json.loads(raw)
    try:
        value = json.loads(raw)
        error = value.get("error", {})
        if isinstance(error, dict):
            row["error"] = {k: error[k] for k in ("code", "reset_seconds", "reset_time", "retry_after_seconds") if k in error}
    except ValueError:
        row["completed_event"] = b'"type":"response.completed"' in raw or b'"type": "response.completed"' in raw
    return row


def main(args):
    root = args.state_dir.resolve()
    root.mkdir(parents=True, mode=0o700, exist_ok=False)
    root.chmod(0o700)
    for name in ("tmp", "cache", "mod", "gopath", "home"):
        (root / name).mkdir(mode=0o700)
    with socket.socket() as check:
        check.bind(("127.0.0.1", args.port))
    repo = Path(__file__).resolve().parents[1]
    # Reuse only the ABI declarations; never edit the shared fixture itself.
    preamble = (repo / "integration/fixture-plugin/main.go").read_text().split('import "C"', 1)[0]
    source = root / "fixture.go"
    source.write_text(preamble + FIXTURE.lstrip("\n"))
    trace = root / "fixture-attempts.jsonl"
    env = dict(os.environ, TMPDIR=str(root / "tmp"), GOCACHE=str(root / "cache"), GOMODCACHE=str(root / "mod"), GOPATH=str(root / "gopath"), GOTOOLCHAIN="local", GO111MODULE="off", CURSOR_COOLDOWN_TRACE=str(trace))
    suffix = ".dylib" if sys.platform == "darwin" else ".dll" if sys.platform == "win32" else ".so"
    library = root / ("cursor-next" + suffix)
    subprocess.run(["go", "build", "-trimpath", "-buildmode=c-shared", "-o", str(library), str(source)], env=env, check=True, timeout=90)
    auth = root / "synthetic-auth.json"
    auth.write_text(json.dumps({"type":"cursor","cursor_auth":{"type":"oauth","access":"synthetic-unused","refresh":"none","expires":9999999999999}}))
    auth.chmod(0o600)
    log = (root / "host.log").open("x")
    proc = subprocess.Popen([sys.executable, str(repo / "integration/start_isolated.py"), "--cpa-binary", str(args.cpa_binary.resolve()), "--plugin", str(library), "--auth-source", str(auth), "--state-dir", str(root / "host"), "--port", str(args.port)], stdout=log, stderr=subprocess.STDOUT, env=env)
    (root / "host.pid").write_text(str(proc.pid) + "\n")
    report = {"cpa_sha256":hashlib.sha256(args.cpa_binary.read_bytes()).hexdigest(),"fixture_sha256":hashlib.sha256(source.read_bytes()).hexdigest(),"port":args.port,"provider_requests":0,"global_cooldown_configuration_changed":False,"cases":[]}
    try:
        base = f"http://127.0.0.1:{args.port}"
        for _ in range(100):
            if proc.poll() is not None:
                raise RuntimeError("isolated CPA exited; inspect private host.log")
            try:
                keys = json.loads((root / "host/keys.json").read_text())
                if len(request(base, keys["client"]).get("data", [])) == 4:
                    break
            except (OSError, ValueError):
                pass
            time.sleep(.1)
        else:
            raise RuntimeError("isolated CPA did not become ready")
        for kind in ("short-sync", "short-stream", "default-sync", "default-stream"):
            model, stream = "cursor/cooldown-" + kind, kind.endswith("stream")
            began = time.monotonic()
            first = request(base, keys["client"], model, stream)
            blocked = request(base, keys["client"], model, stream)
            time.sleep(2.25)
            after = request(base, keys["client"], model, stream)
            attempts = [json.loads(line) for line in trace.read_text().splitlines() if json.loads(line)["model"] == model]
            short = kind.startswith("short")
            passed = (first["status"] == 502 and blocked["status"] == 503 and
                      (after["status"] == 200 if short else after["status"] == 503) and
                      len(attempts) == (2 if short else 1))
            if short:
                passed = passed and blocked["retry_after_header"] in ("1", "2")
                if stream:
                    passed = passed and after.get("completed_event", False)
            else:
                passed = passed and int(blocked["retry_after_header"] or 0) >= 55 and int(after["retry_after_header"] or 0) >= 50
            report["cases"].append({"case":kind,"passed":passed,"first":first,"immediate":blocked,"after_2_25_seconds":after,"elapsed_seconds":round(time.monotonic()-began,3),"fixture_calls":attempts})
        report["passed"] = all(row["passed"] for row in report["cases"])
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=8)
        except subprocess.TimeoutExpired:
            proc.kill(); proc.wait(timeout=5)
        log.close()
        deleted = []
        for path in (auth, root / "host/oauth/cursor-test.json"):
            if path.exists():
                path.unlink(); deleted.append(str(path))
        report["cleanup"] = {"pid":proc.pid,"process_reaped":proc.returncode is not None,"copied_synthetic_credentials_deleted":deleted}
        (root / "result.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa-binary", type=Path, required=True)
    parser.add_argument("--state-dir", type=Path, required=True)
    parser.add_argument("--port", type=int, default=28376)
    sys.exit(main(parser.parse_args()))
