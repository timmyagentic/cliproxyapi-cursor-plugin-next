// Synthetic host compatibility fixture. C ABI declarations derived from this project main.go (MIT).
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"strings"
	"unsafe"
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	C.store_host_api(host)
	plugin.abi_version = 1
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

func encode(v any) []byte { b, _ := json.Marshal(v); return b }
func hostCall(method string, value any) {
	cm := C.CString(method)
	defer C.free(unsafe.Pointer(cm))
	raw := encode(value)
	ptr := C.CBytes(raw)
	defer C.free(ptr)
	var out C.cliproxy_buffer
	C.call_host_api(cm, (*C.uint8_t)(ptr), C.size_t(len(raw)), &out)
	if out.ptr != nil {
		C.free_host_buffer(out.ptr, out.len)
	}
}
func models() any {
	out := []any{}
	for _, name := range []string{"known", "unknown", "zero", "estimated", "deferred", "reasoning-unknown"} {
		out = append(out, map[string]any{"ID": "cursor/compat-" + name, "Object": "model", "OwnedBy": "cursor", "Type": "cursor", "Name": "compat-" + name, "ContextLength": 256000, "SupportedGenerationMethods": []string{"chat"}})
	}
	return map[string]any{"Provider": "cursor", "Models": out}
}
func usage(model string) any {
	reasoningUnknown := strings.HasSuffix(model, "reasoning-unknown")
	if strings.Contains(model, "unknown") && !reasoningUnknown {
		return nil
	}
	u := map[string]any{"prompt_tokens": 140260, "completion_tokens": 669, "total_tokens": 140929, "prompt_tokens_details": map[string]any{"cached_tokens": 140160, "cache_write_tokens": 20}, "completion_tokens_details": map[string]any{"reasoning_tokens": 200}}
	if reasoningUnknown {
		delete(u, "completion_tokens_details")
	}
	if strings.Contains(model, "zero") {
		return map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "prompt_tokens_details": map[string]any{"cached_tokens": 0, "cache_write_tokens": 0}}
	}
	if strings.Contains(model, "estimated") {
		u["cursor_usage_estimated"] = true
	}
	if strings.Contains(model, "deferred") {
		u["cursor_deferred"] = map[string]any{"prompt_tokens": 999999, "completion_tokens": 888888}
	}
	return u
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	var req struct {
		Model, FileName string
		RawJSON         []byte
		CallbackID      string `json:"host_callback_id"`
		StreamID        string `json:"stream_id"`
	}
	_ = json.Unmarshal(C.GoBytes(unsafe.Pointer(request), C.int(requestLen)), &req)
	var result any = map[string]any{}
	switch C.GoString(method) {
	case "plugin.register", "plugin.reconfigure":
		result = map[string]any{"schema_version": 1, "metadata": map[string]any{"Name": "Synthetic usage compatibility fixture", "Version": "0.0.0", "Author": "test", "GitHubRepository": "https://github.com/timmyagentic/cliproxyapi-cursor-plugin-next"}, "capabilities": map[string]any{"auth_provider": true, "model_provider": true, "executor": true, "executor_model_scope": "oauth", "executor_input_formats": []string{"chat-completions"}, "executor_output_formats": []string{"chat-completions"}}}
	case "auth.identifier", "executor.identifier":
		result = map[string]any{"identifier": "cursor"}
	case "auth.parse":
		result = map[string]any{"Handled": true, "Auth": map[string]any{"ID": "synthetic-compat-auth", "Provider": "cursor", "FileName": req.FileName, "StorageJSON": req.RawJSON, "Metadata": map[string]any{"type": "cursor"}}}
	case "model.static", "model.for_auth":
		result = models()
	case "executor.execute":
		body := map[string]any{"id": "chatcmpl-synthetic", "object": "chat.completion", "model": req.Model, "created": 1, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "fixture"}, "finish_reason": "stop"}}}
		if u := usage(req.Model); u != nil {
			body["usage"] = u
		}
		result = map[string]any{"Payload": encode(body)}
	case "executor.execute_stream":
		go func() {
			fields := map[string]any{"host_callback_id": req.CallbackID, "stream_id": req.StreamID}
			chunk := map[string]any{"id": "chatcmpl-synthetic", "object": "chat.completion.chunk", "model": req.Model, "created": 1, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "fixture"}, "finish_reason": nil}}}
			fields["payload"] = []byte("data: " + string(encode(chunk)) + "\n\n")
			hostCall("host.stream.emit", fields)
			chunk["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}
			if u := usage(req.Model); u != nil {
				chunk["usage"] = u
			}
			fields["payload"] = []byte("data: " + string(encode(chunk)) + "\n\n")
			hostCall("host.stream.emit", fields)
			fields["payload"] = []byte("data: [DONE]\n\n")
			hostCall("host.stream.emit", fields)
			delete(fields, "payload")
			hostCall("host.stream.close", fields)
		}()
	}
	out := encode(map[string]any{"ok": true, "result": result})
	response.ptr = C.CBytes(out)
	response.len = C.size_t(len(out))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}
