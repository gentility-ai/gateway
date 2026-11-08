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

static int call_host(const char* method, const uint8_t* request, size_t len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return -1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// The request and response hooks only read: the request hook names the login
// chosen for a request, the response hooks carry its quota headers. Schema 8
// leaves request bodies off all of them, and history off stream chunks.
const registration = `{"schema_version":8,"metadata":{"Name":"gentility-usage","Version":"0.3.0","Author":"Gentility","GitHubRepository":"https://github.com/gentility-ai/gateway","Logo":"","ConfigFields":[]},"capabilities":{"usage_plugin":true,"quota_provider":true,"request_interceptor":true,"request_interceptor_omit_body_before":true,"request_interceptor_omit_body_after":true,"response_interceptor":true,"response_stream_interceptor":true}}`

const quotaDescription = `{"supported_providers":["codex","claude"],"display_name":"Gentility quota watch"}`

var (
	start    sync.Once
	startErr error
	cancel   context.CancelFunc = func() {}
	posted   *poster
	watched  *watcher
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	body := []byte{}
	if request != nil {
		body = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), body)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { stop() }

func nativeHostCall(method string, request []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cRequest *C.uint8_t
	if len(request) > 0 {
		ptr := C.CBytes(request)
		defer C.free(ptr)
		cRequest = (*C.uint8_t)(ptr)
	}
	var response C.cliproxy_buffer
	status := C.call_host(cMethod, cRequest, C.size_t(len(request)), &response)
	var answer []byte
	if response.ptr != nil {
		answer = C.GoBytes(response.ptr, C.int(response.len))
		C.free_host_buffer(response.ptr, response.len)
	}
	if status < 0 {
		return nil, errors.New("the host handed over no callback")
	}
	if len(answer) == 0 && status != 0 {
		return nil, fmt.Errorf("host call %s failed", method)
	}
	return answer, nil
}

// stop ends the poster and the watcher, and saves the quota record before the
// host unloads the plugin.
func stop() {
	cancel()
	if watched != nil {
		watched.save()
	}
}

// begin starts the poster and the quota watcher once. Records spooled before
// a restart are sent on the poster's first flush; the quota record is read
// back from its file beside the spool. Without its URL or spool there is no
// metering, and registration fails so the proxy never serves unmetered.
func begin() error {
	start.Do(func() {
		url, dir := os.Getenv("GENTILITY_USAGE_URL"), os.Getenv("GENTILITY_USAGE_SPOOL")
		if url == "" || dir == "" {
			startErr = errors.New("GENTILITY_USAGE_URL and GENTILITY_USAGE_SPOOL must both be set")
			log.Printf("gentility-usage: %v; refusing to register", startErr)
			return
		}
		posted = newPoster(url, &spool{dir: dir})
		watched = newWatcher(filepath.Join(filepath.Dir(filepath.Clean(dir)), "gentility-quota.json"))
		ctx, stop := context.WithCancel(context.Background())
		cancel = stop
		go posted.run(ctx)
		go watched.run(ctx)
	})
	return startErr
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := begin(); err != nil {
			return errorEnvelope("metering_unconfigured", err.Error()), nil
		}
		return ok(json.RawMessage(registration))
	case pluginabi.MethodUsageHandle:
		if posted == nil {
			return ok(json.RawMessage(`{}`))
		}
		if rec, kept := fromHost(request); kept {
			if err := posted.spool.append(rec); err != nil {
				log.Printf("gentility-usage: spool %s: %v", rec.RequestID, err)
			}
			posted.poke()
		}
		return ok(json.RawMessage(`{}`))
	case pluginabi.MethodQuotaIdentifier:
		return ok(json.RawMessage(`{"identifier":"gentility-usage"}`))
	case pluginabi.MethodQuotaDescribe:
		return ok(json.RawMessage(quotaDescription))
	case pluginabi.MethodQuotaFetch:
		var asked struct {
			AuthIndex string `json:"auth_index"`
		}
		if err := json.Unmarshal(request, &asked); err != nil || watched == nil {
			return ok(json.RawMessage(`{}`))
		}
		raw, _ := json.Marshal(watched.fetch(asked.AuthIndex))
		return ok(raw)
	case pluginabi.MethodRequestInterceptBefore:
		return ok(json.RawMessage(`{}`))
	case pluginabi.MethodRequestInterceptAfter:
		if watched != nil {
			watched.noteServing(request)
		}
		return ok(json.RawMessage(`{}`))
	case pluginabi.MethodResponseInterceptAfter, pluginabi.MethodResponseInterceptStreamChunk:
		if watched != nil {
			watched.observeReply(request)
		}
		return ok(json.RawMessage(`{}`))
	case pluginabi.MethodPluginShutdown:
		stop()
		return ok(json.RawMessage(`{}`))
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func ok(result json.RawMessage) ([]byte, error) {
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
