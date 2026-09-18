# CPA Plugins

CLIProxyAPI (CPA) 插件集合。**不改 CPA 源码** —— 全部通过官方 C-ABI 插件机制实现。

目标环境：server-2 (154.201.88.223)，CLIProxyAPI v7.3.6 (commit `8c664b2`)，容器名 `cpa`。

## 插件列表

| 插件 | 作用 | 状态 |
|---|---|---|
| [`plugin-codex-relay`](plugins/plugin-codex-relay/) | 把 Codex OAuth 凭据的上游改到指定网关（如 codex-relay），替代改源码或 `codex-api-key.base-url` | 已验证 |
| [`plugin-o`](plugins/plugin-o/) | 检测「请求模型」与「上游实际服务模型」不一致 | 已部署 |

## 核心机制

CPA 决定是否走自定义上游的关键在 `codexCreds()`：

```go
func codexCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
    apiKey  = a.Attributes["api_key"]
    baseURL = a.Attributes["base_url"]   // ← 这个属性决定上游
    ...
}
```

各 Codex executor（HTTP / 流式 / WebSocket / 图片）都调用它，若 `base_url` 为空才回落到官方
`https://chatgpt.com/backend-api/codex`。

所以**插件只要能在 auth 上注入 `Attributes["base_url"]`，就等于改了上游**，无需碰 CPA 一行代码。

注入点是 `AuthProvider.ParseAuth` —— CPA 解析 auth 目录下的 JSON 文件时会回调插件：

```
CPA 扫描 auth-dir
   ↓ auth.parse（每个 JSON 文件）
插件返回 AuthData{Attributes: {"base_url": "...", "header:X": "..."}}
   ↓
CPA 用这些属性构造 Auth
   ↓ codexCreds() 读到 base_url
请求发往自定义上游
```

## 构建

**不要在 Windows 上交叉编译 c-shared** —— 需要 cgo + 交叉 gcc，会因 `setenv`/`sys/mman.h` 缺失失败。
在服务器上的 Go 容器里构建：

```sh
docker run --rm --memory=1g --memory-swap=1g \
  -v /opt:/opt -w /opt/cpabuild/<plugin-dir> golang:1.26 sh -c '
    export GOFLAGS=-mod=mod GOCACHE=/opt/cpabuild/gocache GOPATH=/opt/cpabuild/gopath
    go build -buildmode=c-shared -o /opt/cpabuild/<plugin>.so .
  '
```

> 服务器内存 1.9G，务必用 `--memory` 限流，否则 Go 编译可能 OOM。
> 插件尽量只依赖标准库（`encoding/json` + `fmt`），不 import CPA SDK —— 编译快、体积小、无依赖图。

## 部署

```sh
# 插件目录优先级：plugins/<goos>/<goarch>/ 高于 plugins/
D=/root/.cli-proxy-api/plugins/linux/amd64
docker exec cpa mkdir -p $D
docker cp <plugin>.so cpa:$D/<plugin>.so
```

在 `config.yaml` 启用：

```yaml
plugins:
  enabled: true
  dir: "/root/.cli-proxy-api/plugins"
  configs:
    <plugin-id>:
      enabled: true
      # 插件自定义字段
```

然后 `docker restart cpa`，用日志确认加载：

```sh
docker logs cpa 2>&1 | grep -i 'plugin'
# pluginhost: plugin loaded plugin_id=...
# pluginhost: plugin registered plugin_id=... plugin_name=...
```

## 坑

- **插件 ID 来自文件名**：`foo.so` → `plugin-id = foo`（需匹配 `[a-z0-9-]`）。文件放错目录则 config 里的 ID 对不上。
- **`[]byte` 字段是 base64**：ABI 的 `RawJSON`/`StorageJSON` 在 JSON 里是 `[]byte`，必须用 `[]byte` 接收让
  `encoding/json` 自动解码。用 `json.RawMessage` 会拿到 base64 字符串导致解析失败（表现为静默跳过所有文件）。
- **注册了 capability 就要实现全部方法**：声明 `auth_provider` 后，`auth.identifier`/`auth.parse`/
  `auth.login.start`/`auth.login.poll`/`auth.refresh` 都可能被回调，未实现的方法要在 default 分支返回
  合法空信封，不要报错。
- **配置是嵌套 YAML，不是扁平的**：插件收到的 `config_yaml` 是 map 序列化的 YAML，带缩进。
  手写解析要按缩进判断层级；简单起见用 `gopkg.in/yaml.v3`（但会增加依赖）。
- 改配置后 CPA 会热重载，插件会 unload→load，日志里出现多轮 `configured` 属正常。
