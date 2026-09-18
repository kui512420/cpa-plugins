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
	"sort"
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

type managementRequest struct {
	Method  string              `json:"Method"`
	Path    string              `json:"Path"`
	Headers map[string][]string `json:"Headers,omitempty"`
	Query   map[string][]string `json:"Query,omitempty"`
	Body    []byte              `json:"Body,omitempty"`
}

type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body"`
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
	Enabled  bool
	BaseURL  string
	Mode     string
	Headers  map[string]string
	Password string
}

// accessCookieName carries the plugin's own session token. The management panel
// renders plugin pages inside an iframe and never forwards the CPA management
// key, so the plugin issues its own cookie instead.
const accessCookieName = "cpa_codex_relay"

var sessionState = struct {
	mu     sync.Mutex
	tokens map[string]time.Time
}{tokens: make(map[string]time.Time)}

const sessionTTL = 12 * time.Hour

type registration struct {
	SchemaVersion uint32          `json:"schema_version"`
	Metadata      map[string]any  `json:"metadata"`
	Capabilities  map[string]bool `json:"capabilities"`
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
		return managementHandle(request)
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

// parsePluginConfig reads the config block the host emits for this plugin.
// The host serialises the config map, so keys keep their nesting and headers
// appear as an indented sub-map.
func parsePluginConfig(raw []byte) pluginConfig {
	cfg := pluginConfig{Headers: map[string]string{}, Mode: "all"}
	if len(raw) == 0 {
		return cfg
	}
	if decoded, ok := decodeJSONObject(raw); ok {
		return configFromMap(decoded)
	}
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
			if indent == 0 && strings.EqualFold(key, "headers") {
				inHeaders = true
			}
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
			case "password":
				cfg.Password = value
			case "headers":
				inHeaders = strings.TrimSpace(value) == ""
			}
			continue
		}
		if inHeaders {
			cfg.Headers[key] = value
		}
	}
	return cfg
}

// decodeJSONObject reads the config body when the host hands over JSON.
func decodeJSONObject(raw []byte) (map[string]any, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") {
		return nil, false
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal([]byte(trimmed), &decoded); errUnmarshal != nil {
		return nil, false
	}
	return decoded, true
}

func configFromMap(decoded map[string]any) pluginConfig {
	cfg := pluginConfig{Headers: map[string]string{}, Mode: "all"}
	if v, ok := decoded["enabled"].(bool); ok {
		cfg.Enabled = v
	}
	if v, ok := decoded["base_url"].(string); ok {
		cfg.BaseURL = strings.TrimSpace(v)
	}
	if v, ok := decoded["mode"].(string); ok && strings.TrimSpace(v) != "" {
		cfg.Mode = strings.TrimSpace(v)
	}
	if v, ok := decoded["password"].(string); ok {
		cfg.Password = strings.TrimSpace(v)
	}
	if headers, ok := decoded["headers"].(map[string]any); ok {
		for name, value := range headers {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if text, isString := value.(string); isString {
				cfg.Headers[name] = strings.TrimSpace(text)
			}
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
			"Version":          "0.2.0",
			"Author":           "kui",
			"GitHubRepository": "https://github.com/kui512420/cpa-plugins",
			"ConfigFields": []map[string]any{
				{"Name": "enabled", "Type": "boolean", "Description": "Adopt codex OAuth auth files and route them to a custom upstream."},
				{"Name": "base_url", "Type": "string", "Description": "Upstream Codex base URL, e.g. http://172.19.0.1:8320/backend-api/codex"},
				{"Name": "mode", "Type": "enum", "EnumValues": []string{"all", "optin"}, "Description": "all = adopt every codex OAuth file; optin = only marked files."},
				{"Name": "headers", "Type": "object", "Description": "Extra upstream headers injected on every adopted auth request."},
				{"Name": "password", "Type": "string", "Description": "Access password for the browser configuration page. Leave empty to allow open access."},
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
		"routes": []map[string]any{
			{
				"Method":      "GET",
				"Path":        "/codex-relay/ui",
				"Description": "Codex Relay Router configuration page.",
			},
			{
				"Method":      "POST",
				"Path":        "/codex-relay/config",
				"Description": "Update Codex Relay Router runtime configuration.",
			},
			{
				"Method":      "GET",
				"Path":        "/codex-relay/status",
				"Description": "Codex Relay Router status as JSON.",
			},
		},
		"resources": []map[string]string{
			{
				"Path":        "/home",
				"Menu":        "Codex 上游路由",
				"Description": "Codex Relay Router 配置页。",
			},
			{
				"Path":        "/ui",
				"Menu":        "",
				"Description": "Codex Relay Router configuration page (rendered in the panel iframe).",
			},
		},
	}
}

// resourceBase is the prefix CPA serves plugin resource routes under.
const resourceBase = "/v0/resource/plugins/plugin-codex-relay"

func managementHandle(raw []byte) ([]byte, error) {
	var req managementRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")

	// Browser-reachable configuration page. CPA only routes GET to resource
	// routes, so login and save are expressed as GET + query too.
	switch {
	case method == "GET" && path == resourceBase+"/ui":
		return handleUI(&req)
	case method == "GET" && path == resourceBase+"/home":
		return handleUI(&req)
	case method == "GET" && path == "/v0/management/codex-relay/status":
		return okEnvelope(jsonResponse(statusReport()))
	default:
		return okEnvelope(jsonErrorResponse(404, "not found: "+method+" "+path))
	}
}

// handleUI gates the configuration page behind the plugin's own access
// password. The management panel cannot pass the CPA management key into its
// iframe, so this is the only way to make the page usable from a browser.
func handleUI(req *managementRequest) ([]byte, error) {
	query := queryFirst(req.Query)
	expected := currentPassword()

	// No password configured: the page is open, matching the plugin's
	// "opt-in protection" model.
	if expected == "" {
		if _, hasSave := query["save"]; hasSave {
			return saveFromQuery(query, req)
		}
		return okEnvelope(htmlResponse(renderUI("", true)))
	}

	if token := cookieValue(req.Headers, accessCookieName); token != "" && sessionValid(token) {
		if _, hasSave := query["save"]; hasSave {
			return saveFromQuery(query, req)
		}
		return okEnvelope(htmlResponse(renderUI("", true)))
	}

	if provided, ok := query["password"]; ok {
		if constantTimeEqual(provided, expected) {
			token := newSessionToken()
			body := renderUI("已验证，配置已可编辑。", true)
			response := htmlResponse(body)
			response.Headers["Set-Cookie"] = []string{cookieHeader(token)}
			return okEnvelope(response)
		}
		return okEnvelope(htmlResponse(renderLogin("口令不正确。")))
	}

	return okEnvelope(htmlResponse(renderLogin("")))
}

func saveFromQuery(query map[string]string, req *managementRequest) ([]byte, error) {
	expected := currentPassword()
	if expected != "" {
		token := cookieValue(req.Headers, accessCookieName)
		if token == "" || !sessionValid(token) {
			return okEnvelope(htmlResponse(renderLogin("会话已过期，请重新输入口令。")))
		}
	}

	cfgState.mu.Lock()
	next := cfgState.current
	if next.Headers == nil {
		next.Headers = map[string]string{}
	}
	next.Enabled = truthy(query["enabled"])
	if raw, ok := query["base_url"]; ok {
		next.BaseURL = strings.TrimSpace(raw)
	}
	if raw, ok := query["mode"]; ok && strings.TrimSpace(raw) != "" {
		next.Mode = strings.TrimSpace(raw)
	}
	if raw, ok := query["headers"]; ok {
		next.Headers = parseHeaderBlock(raw)
	}
	cfgState.current = next
	cfgState.mu.Unlock()

	logf("ui update enabled=%v base_url=%q mode=%q headers=%d",
		next.Enabled, next.BaseURL, next.Mode, len(next.Headers))

	response := htmlResponse(renderUI("已保存并立即生效。重启 CPA 后以 config.yaml 为准，请同步修改配置文件以持久化。", true))
	return okEnvelope(response)
}

func currentPassword() string {
	cfgState.mu.RLock()
	defer cfgState.mu.RUnlock()
	return strings.TrimSpace(cfgState.current.Password)
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	mismatch := byte(0)
	for i := 0; i < len(left); i++ {
		mismatch |= left[i] ^ right[i]
	}
	return mismatch == 0
}

func newSessionToken() string {
	buf := make([]byte, 32)
	if file, errOpen := os.Open("/dev/urandom"); errOpen == nil {
		if _, errRead := file.Read(buf); errRead != nil {
			fallbackFill(buf)
		}
		_ = file.Close()
	} else {
		fallbackFill(buf)
	}
	token := fmt.Sprintf("%x", buf)
	sessionState.mu.Lock()
	sessionState.tokens[token] = time.Now().Add(sessionTTL)
	sessionState.mu.Unlock()
	return token
}

// fallbackFill is only reached when /dev/urandom is unavailable; it mixes the
// nanosecond clock so tokens stay unpredictable enough for a local admin page.
func fallbackFill(buf []byte) {
	seed := time.Now().UnixNano()
	for i := range buf {
		seed = seed*6364136223846793005 + 1442695040888963407
		buf[i] = byte(seed >> 33)
	}
}

func sessionValid(token string) bool {
	sessionState.mu.Lock()
	defer sessionState.mu.Unlock()
	expires, ok := sessionState.tokens[token]
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		delete(sessionState.tokens, token)
		return false
	}
	return true
}

func cookieHeader(token string) string {
	return fmt.Sprintf("%s=%s; Path=%s; Max-Age=%d; HttpOnly; SameSite=Lax",
		accessCookieName, token, resourceBase, int(sessionTTL.Seconds()))
}

func cookieValue(headers map[string][]string, name string) string {
	if headers == nil {
		return ""
	}
	raw, ok := headers["Cookie"]
	if !ok {
		raw, ok = headers["cookie"]
		if !ok {
			return ""
		}
	}
	for _, line := range raw {
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			if !strings.HasPrefix(part, name+"=") {
				continue
			}
			return strings.TrimSpace(strings.TrimPrefix(part, name+"="))
		}
	}
	return ""
}

func queryFirst(query map[string][]string) map[string]string {
	out := map[string]string{}
	for key, values := range query {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on", "true", "1", "yes":
		return true
	}
	return false
}

// parseHeaderBlock reads a simple "Name: value" list, one entry per line.
func parseHeaderBlock(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		entry := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if entry == "" {
			continue
		}
		key, value, hasValue := splitYAMLKeyValue(entry)
		if !hasValue || key == "" {
			continue
		}
		out[key] = trimYAMLQuotes(strings.TrimSpace(value))
	}
	return out
}

func statusReport() map[string]any {
	cfgState.mu.RLock()
	cfg := cfgState.current
	cfgState.mu.RUnlock()
	counters.mu.Lock()
	defer counters.mu.Unlock()
	return map[string]any{
		"enabled":        cfg.Enabled,
		"base_url":       strings.TrimSpace(cfg.BaseURL),
		"mode":           strings.TrimSpace(cfg.Mode),
		"headers":        cfg.Headers,
		"adopted_auths":  counters.adopted,
		"skipped_files":  counters.skipped,
		"last_auth_file": counters.lastFile,
		"last_observed":  counters.lastAt,
	}
}

func jsonResponse(report map[string]any) managementResponse {
	body, errMarshal := json.MarshalIndent(report, "", "  ")
	if errMarshal != nil {
		body = []byte("{}")
	}
	return managementResponse{
		StatusCode: 200,
		Headers:    map[string][]string{"content-type": {"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func jsonErrorResponse(status int, message string) managementResponse {
	body, _ := json.Marshal(map[string]any{"error": message})
	return managementResponse{
		StatusCode: status,
		Headers:    map[string][]string{"content-type": {"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func htmlResponse(body string) managementResponse {
	return managementResponse{
		StatusCode: 200,
		Headers:    map[string][]string{"content-type": {"text/html; charset=utf-8"}},
		Body:       []byte(body),
	}
}

// renderLogin asks for the plugin access password.
func renderLogin(message string) string {
	notice := ""
	if message != "" {
		notice = `<div class="notice">` + htmlEscape(message) + `</div>`
	}
	body := fmt.Sprintf(`
%s
<div class="card">
<h1>Codex 上游路由</h1>
<p class="sub">此页面受插件访问口令保护。请输入 <code>plugins.configs.plugin-codex-relay.password</code> 中配置的口令。</p>
<form method="get" action="%s/ui">
  <div class="row"><span class="lbl">访问口令</span>
    <input type="password" name="password" autocomplete="current-password" autofocus></div>
  <button type="submit">进入</button>
</form>
</div>`, notice, resourceBase)
	return pageShell(body)
}

func renderUI(savedMessage string, editable bool) string {
	cfgState.mu.RLock()
	cfg := cfgState.current
	cfgState.mu.RUnlock()
	counters.mu.Lock()
	adopted := counters.adopted
	skipped := counters.skipped
	lastFile := counters.lastFile
	lastAt := counters.lastAt
	counters.mu.Unlock()

	enabledChecked := ""
	if cfg.Enabled {
		enabledChecked = " checked"
	}
	allSelected := ""
	optinSelected := ""
	if strings.EqualFold(strings.TrimSpace(cfg.Mode), "optin") {
		optinSelected = " selected"
	} else {
		allSelected = " selected"
	}

	names := make([]string, 0, len(cfg.Headers))
	for name := range cfg.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	headerLines := make([]string, 0, len(names))
	for _, name := range names {
		headerLines = append(headerLines, name+": "+cfg.Headers[name])
	}

	notice := ""
	if savedMessage != "" {
		notice = `<div class="notice">` + htmlEscape(savedMessage) + `</div>`
	}

	formBlock := ""
	if editable {
		formBlock = fmt.Sprintf(`
<form method="get" action="%s/ui">
  <input type="hidden" name="save" value="1">
  <label class="row"><input type="checkbox" name="enabled"%s> 启用接管</label>
  <div class="row"><span class="lbl">上游地址 base_url</span>
    <input type="text" name="base_url" value="%s" placeholder="http://172.19.0.1:8320/backend-api/codex"></div>
  <div class="row"><span class="lbl">接管范围 mode</span>
    <select name="mode"><option value="all"%s>all — 接管所有 codex OAuth 文件</option><option value="optin"%s>optin — 仅接管标记文件</option></select></div>
  <div class="row"><span class="lbl">附加请求头（每行一条 Name: value）</span>
    <textarea name="headers" rows="4" placeholder="X-Foo: bar">%s</textarea></div>
  <button type="submit">保存</button>
</form>`, resourceBase, enabledChecked, htmlEscape(cfg.BaseURL), allSelected, optinSelected,
			htmlEscape(strings.Join(headerLines, "\n")))
	} else {
		formBlock = `<p class="sub">当前为只读视图。</p>`
	}

	body := fmt.Sprintf(`
%s
<div class="card">
<h1>Codex 上游路由</h1>
<p class="sub">把 Codex OAuth 凭据的请求改发到指定网关，无需修改 CPA 源码。保存后立即生效，但仅存在于内存中；重启后以 <code>config.yaml</code> 为准。</p>
%s
</div>
<div class="card">
<h2>当前状态</h2>
<table><tbody>
<tr><th>启用</th><td>%s</td></tr>
<tr><th>上游地址</th><td><code>%s</code></td></tr>
<tr><th>接管范围</th><td><code>%s</code></td></tr>
<tr><th>已接管次数</th><td>%s</td></tr>
<tr><th>跳过文件数</th><td>%s</td></tr>
<tr><th>最近文件</th><td class="dim">%s</td></tr>
<tr><th>最近时间</th><td class="dim">%s</td></tr>
</tbody></table>
</div>`, notice, formBlock,
		boolText(cfg.Enabled), htmlEscape(orDefault(cfg.BaseURL, "(未设置)")),
		htmlEscape(orDefault(cfg.Mode, "all")),
		fmt.Sprintf("%d", adopted), fmt.Sprintf("%d", skipped),
		htmlEscape(orDefault(lastFile, "—")), htmlEscape(orDefault(lastAt, "—")))

	return pageShell(body)
}

func pageShell(body string) string {
	return `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>Codex 上游路由</title>
<style>
:root{--bg:#f7f8fa;--card:#fff;--bd:#e5e7eb;--tx:#111827;--dim:#6b7280;--ac:#2563eb;--warn:#b45309;--wbg:#fef3c7}
*{box-sizing:border-box}
body{margin:0;padding:28px;background:var(--bg);color:var(--tx);
font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
h1{font-size:20px;margin:0 0 6px}
h2{font-size:15px;margin:0 0 12px;color:#374151}
.sub{color:var(--dim);font-size:13px;margin:0 0 18px}
.card{background:var(--card);border:1px solid var(--bd);border-radius:10px;padding:18px 20px;margin-bottom:14px;max-width:760px}
.row{display:block;margin-bottom:14px}
.lbl{display:block;font-size:13px;color:#374151;margin-bottom:5px;font-weight:600}
input[type=text],textarea,select{width:100%;padding:8px 10px;border:1px solid var(--bd);border-radius:7px;
font:13px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;background:#fff}
textarea{resize:vertical}
input[type=checkbox]{width:16px;height:16px;vertical-align:-2px;margin-right:6px}
button{background:var(--ac);color:#fff;border:0;border-radius:7px;padding:9px 18px;font-size:14px;cursor:pointer}
button:hover{filter:brightness(1.08)}
table{width:100%;border-collapse:collapse;font-size:13px}
th,td{text-align:left;padding:8px 10px;border-bottom:1px solid var(--bd)}
th{width:130px;color:#374151;font-weight:600;background:#f9fafb}
tr:last-child td,tr:last-child th{border-bottom:none}
code{background:#f3f4f6;padding:2px 6px;border-radius:4px;font-size:12.5px;
font-family:ui-monospace,SFMono-Regular,Menlo,monospace;word-break:break-all}
.dim{color:var(--dim);font-family:ui-monospace,Menlo,monospace;font-size:12px}
.notice{background:var(--wbg);color:var(--warn);border-radius:8px;padding:10px 14px;margin-bottom:14px;
font-size:13px;max-width:760px}
.pill{display:inline-block;padding:2px 10px;border-radius:999px;font-size:12px;font-weight:600}
.pill.on{background:#d1fae5;color:#065f46}
.pill.off{background:#e5e7eb;color:#6b7280}
.note-line{margin:12px 0 0}
.btn{display:inline-block;background:var(--ac);color:#fff;text-decoration:none;
border-radius:7px;padding:9px 18px;font-size:14px}
.btn:hover{filter:brightness(1.08)}
</style></head><body>` + body + `</body></html>`
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func boolText(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

func htmlEscape(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	value = strings.ReplaceAll(value, `"`, "&quot;")
	value = strings.ReplaceAll(value, "'", "&#39;")
	return value
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
