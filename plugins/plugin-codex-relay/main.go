package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
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
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const (
	abiVersion    uint32 = 1
	schemaVersion uint32 = 6
)

// RPC method names, mirrored from sdk/pluginabi.
const (
	methodPluginRegister    = "plugin.register"
	methodPluginReconfigure = "plugin.reconfigure"
	methodAuthIdentifier    = "auth.identifier"
	methodAuthParse         = "auth.parse"
	methodAuthRefresh       = "auth.refresh"
	methodManagementReg     = "management.register"
	methodManagementHandle  = "management.handle"
)

// providerCodex is the built-in provider key whose auth files this plugin adopts.
const providerCodex = "codex"

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envErr         `json:"error,omitempty"`
}

type envErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

// authParseRequest mirrors pluginapi.AuthParseRequest.
// RawJSON is []byte (not json.RawMessage) so encoding/json performs the
// base64 round-trip the host uses for byte-slice fields.
type authParseRequest struct {
	Provider string `json:"Provider"`
	Path     string `json:"Path"`
	FileName string `json:"FileName"`
	RawJSON  []byte `json:"RawJSON"`
}

// authData mirrors pluginapi.AuthData.
type authData struct {
	Provider    string            `json:"Provider"`
	ID          string            `json:"ID,omitempty"`
	FileName    string            `json:"FileName,omitempty"`
	Label       string            `json:"Label,omitempty"`
	StorageJSON []byte            `json:"StorageJSON,omitempty"`
	Metadata    map[string]any    `json:"Metadata,omitempty"`
	Attributes  map[string]string `json:"Attributes,omitempty"`
}

type authParseResponse struct {
	Handled bool     `json:"Handled"`
	Auth    authData `json:"Auth"`
}

type authRefreshResponse struct {
	Auth authData `json:"Auth"`
}

type pluginConfig struct {
	Enabled bool
	BaseURL string
	Mode    string
	Headers map[string]string
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      map[string]any         `json:"metadata"`
	Capabilities  map[string]bool        `json:"capabilities"`
}

var cfgState = struct {
	mu      sync.RWMutex
	current pluginConfig
}{}

var counters = struct {
	mu       sync.Mutex
	adopted  int
	skipped  int
	lastFile string
	lastAt   string
}{}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(abiVersion)
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
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case methodAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": providerCodex})
	case methodAuthParse:
		return adoptCodexAuth(request)
	case methodAuthRefresh:
		return passthroughRefresh()
	case methodManagementReg:
		return okEnvelope(managementRegistration())
	case methodManagementHandle:
		return managementHandle()
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	parsed := parsePluginConfig(req.ConfigYAML)
	cfgState.mu.Lock()
	cfgState.current = parsed
	cfgState.mu.Unlock()
	logf("configured enabled=%v base_url=%q mode=%q headers=%d",
		parsed.Enabled, parsed.BaseURL, parsed.Mode, len(parsed.Headers))
	return nil
}

// parsePluginConfig reads the flat plugin config keys emitted by the host.
func parsePluginConfig(raw []byte) pluginConfig {
	cfg := pluginConfig{Headers: map[string]string{}}
	inHeaders := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		indent := len(trimmed) - len(strings.TrimLeft(trimmed, " \t"))
		entry := strings.TrimSpace(trimmed)
		if strings.HasPrefix(entry, "- ") {
			continue
		}
		key, value, hasValue := splitYAMLKeyValue(entry)
		if !hasValue {
			continue
		}
		value = trimYAMLQuotes(strings.TrimSpace(value))
		if indent == 0 {
			inHeaders = false
			switch strings.ToLower(key) {
			case "enabled":
				cfg.Enabled = strings.EqualFold(value, "true")
			case "base_url", "base-url":
				cfg.BaseURL = value
			case "mode":
				cfg.Mode = value
			case "headers":
				inHeaders = true
			}
			continue
		}
		if inHeaders {
			cfg.Headers[key] = value
		}
	}
	return cfg
}

func splitYAMLKeyValue(entry string) (string, string, bool) {
	index := strings.Index(entry, ":")
	if index < 0 {
		return "", "", false
	}
	return strings.TrimSpace(entry[:index]), entry[index+1:], true
}

func trimYAMLQuotes(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: map[string]any{
			"Name":             "Codex Relay Router",
			"Version":          "0.1.0",
			"Author":           "kui",
			"GitHubRepository": "https://github.com/router-for-me/CLIProxyAPI",
			"ConfigFields": []map[string]any{
				{"Name": "enabled", "Type": "boolean", "Description": "Adopt codex OAuth auth files and route them to a custom upstream."},
				{"Name": "base_url", "Type": "string", "Description": "Upstream Codex base URL, e.g. http://172.19.0.1:8320/backend-api/codex"},
				{"Name": "mode", "Type": "enum", "EnumValues": []string{"all", "optin"}, "Description": "all = adopt every codex OAuth file; optin = only marked files."},
				{"Name": "headers", "Type": "object", "Description": "Extra upstream headers injected on every adopted auth request."},
			},
		},
		Capabilities: map[string]bool{
			"auth_provider":  true,
			"management_api": true,
		},
	}
}

// adoptCodexAuth takes over parsing of codex OAuth files and injects the relay
// base URL as an auth attribute. The built-in codex executor reads that
// attribute through codexCreds(), so no CPA source change is required.
func adoptCodexAuth(raw []byte) ([]byte, error) {
	var req authParseRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if !strings.EqualFold(strings.TrimSpace(req.Provider), providerCodex) {
		return okEnvelope(authParseResponse{})
	}

	cfgState.mu.RLock()
	cfg := cfgState.current
	cfgState.mu.RUnlock()

	if !cfg.Enabled {
		markSkipped(req.FileName, "disabled")
		return okEnvelope(authParseResponse{})
	}

	var metadata map[string]any
	if len(req.RawJSON) > 0 {
		if errMeta := json.Unmarshal(req.RawJSON, &metadata); errMeta != nil {
			markSkipped(req.FileName, "unparsable")
			return okEnvelope(authParseResponse{})
		}
	}
	if !looksLikeCodexOAuth(metadata) {
		markSkipped(req.FileName, "not-codex-oauth")
		return okEnvelope(authParseResponse{})
	}

	target := ""
	if own, ok := metadata["base_url"].(string); ok {
		target = strings.TrimSpace(own)
	}
	if target == "" {
		if strings.EqualFold(strings.TrimSpace(cfg.Mode), "optin") && !optedIn(metadata) {
			markSkipped(req.FileName, "optin-unmarked")
			return okEnvelope(authParseResponse{})
		}
		target = strings.TrimSpace(cfg.BaseURL)
	}
	if target == "" {
		markSkipped(req.FileName, "no-base-url")
		return okEnvelope(authParseResponse{})
	}

	attributes := map[string]string{"base_url": target}
	for name, value := range cfg.Headers {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		attributes["header:"+name] = strings.TrimSpace(value)
	}
	if planType, ok := metadata["plan_type"].(string); ok {
		if trimmed := strings.TrimSpace(planType); trimmed != "" {
			attributes["plan_type"] = trimmed
		}
	}

	data := authData{
		Provider:    providerCodex,
		FileName:    strings.TrimSpace(req.FileName),
		Label:       authLabel(metadata, req.FileName),
		StorageJSON: append([]byte(nil), req.RawJSON...),
		Metadata:    metadata,
		Attributes:  attributes,
	}

	markAdopted(req.FileName, target)
	return okEnvelope(authParseResponse{Handled: true, Auth: data})
}

// passthroughRefresh keeps the built-in refresh path intact. An empty payload
// makes the host merge the existing metadata and attributes, which preserves
// the injected base_url across refresh cycles.
func passthroughRefresh() ([]byte, error) {
	return okEnvelope(authRefreshResponse{Auth: authData{Provider: providerCodex}})
}

func looksLikeCodexOAuth(metadata map[string]any) bool {
	if len(metadata) == 0 {
		return false
	}
	if kind, ok := metadata["type"].(string); ok {
		if strings.EqualFold(strings.TrimSpace(kind), providerCodex) {
			return true
		}
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token"} {
		if value, ok := metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func optedIn(metadata map[string]any) bool {
	if value, ok := metadata["relay"].(bool); ok {
		return value
	}
	if value, ok := metadata["relay"].(string); ok {
		return strings.EqualFold(strings.TrimSpace(value), "true")
	}
	return false
}

func authLabel(metadata map[string]any, fileName string) string {
	if email, ok := metadata["email"].(string); ok && strings.TrimSpace(email) != "" {
		return strings.TrimSpace(email)
	}
	return strings.TrimSpace(fileName)
}

func markAdopted(fileName, target string) {
	counters.mu.Lock()
	counters.adopted++
	counters.lastFile = fileName
	counters.lastAt = time.Now().UTC().Format(time.RFC3339)
	counters.mu.Unlock()
	logf("adopted file=%s base_url=%s", fileName, target)
}

func markSkipped(fileName, reason string) {
	if strings.TrimSpace(fileName) == "" {
		return
	}
	counters.mu.Lock()
	counters.skipped++
	counters.mu.Unlock()
	logf("skipped file=%s reason=%s", fileName, reason)
}

func managementRegistration() map[string]any {
	return map[string]any{
		"resources": []map[string]string{
			{
				"Path":        "/status",
				"Menu":        "Codex Relay Router",
				"Description": "Shows the relay base URL adopted by codex OAuth credentials.",
			},
		},
	}
}

func managementHandle() ([]byte, error) {
	cfgState.mu.RLock()
	cfg := cfgState.current
	cfgState.mu.RUnlock()
	counters.mu.Lock()
	report := map[string]any{
		"enabled":        cfg.Enabled,
		"base_url":       strings.TrimSpace(cfg.BaseURL),
		"mode":           strings.TrimSpace(cfg.Mode),
		"headers":        cfg.Headers,
		"adopted_auths":  counters.adopted,
		"skipped_files":  counters.skipped,
		"last_auth_file": counters.lastFile,
		"last_observed":  counters.lastAt,
	}
	counters.mu.Unlock()
	return okEnvelope(managementResponse(report))
}

func managementResponse(report map[string]any) map[string]any {
	body, errMarshal := json.MarshalIndent(report, "", "  ")
	if errMarshal != nil {
		body = []byte("{}")
	}
	return map[string]any{
		"StatusCode": 200,
		"Headers":    map[string][]string{"content-type": {"application/json; charset=utf-8"}},
		"Body":       body,
	}
}

func logf(format string, args ...any) {
	_, _ = os.Stderr.WriteString("[plugin-codex-relay] " + fmt.Sprintf(format, args...) + "\n")
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envErr{Code: code, Message: message}})
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
