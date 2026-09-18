# Codex Relay Router

把 Codex OAuth 凭据的上游改到指定网关，**不需要改 CPA 源码**，也不需要
`codex-api-key.base-url` 配置。

## 原理

CPA 的 `codexCreds()` 从 auth 属性读上游地址：

```go
baseURL = a.Attributes["base_url"]   // 为空才回落 https://chatgpt.com/backend-api/codex
```

本插件实现 `AuthProvider`，在 CPA 解析 auth 目录下的 JSON 时接管（`auth.parse`），
返回一个带 `Attributes["base_url"]` 的 `AuthData`。CPA 后续用这个 auth 发请求时，
Codex executor 就会把流量打到指定网关。

## 配置

```yaml
plugins:
  configs:
    plugin-codex-relay:
      enabled: true
      base_url: "http://172.19.0.1:8320/backend-api/codex"
      mode: "all"      # all | optin
      headers:         # 可选，额外上游请求头
        # X-Foo: bar
```

| 字段 | 说明 |
|---|---|
| `enabled` | 关闭则不接管，回落到 CPA 内置解析 |
| `base_url` | 目标上游地址 |
| `mode` | `all` = 接管所有 codex OAuth 文件；`optin` = 只接管标记了 `"relay": true` 的文件 |
| `headers` | 注入 `header:<name>` 属性，随请求发往上游 |

若 auth 文件自身带 `base_url` 字段，则以文件内的为准（方便按账号覆盖）。

## Refresh 处理

`auth.refresh` 返回空 payload，让 CPA 合并原有 metadata 与 attributes，
从而**保留注入的 `base_url`**。若在此返回新 auth 而不带该属性，刷新后路由会回落。

## 验证

部署后重启，日志应出现 `adopted` 而非 `skipped`：

```sh
docker logs cpa 2>&1 | grep 'plugin-codex-relay'
# [plugin-codex-relay] adopted file=codex-xxx.json base_url=http://172.19.0.1:8320/backend-api/codex
```

若全是 `unparsable`，通常是 ABI 的 `[]byte` 字段没按 base64 解码（见仓库 README 的坑）。

管理面板资源：`/v0/resource/plugins/plugin-codex-relay/status`
