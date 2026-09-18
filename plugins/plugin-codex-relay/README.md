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

## 管理界面

| 路由 | 鉴权 | 说明 |
|---|---|---|
| `GET /v0/management/codex-relay/ui` | 需要 | 配置页面（HTML 表单） |
| `POST /v0/management/codex-relay/config` | 需要 | 保存配置 |
| `GET /v0/management/codex-relay/status` | 需要 | 当前状态 JSON |
| `GET /v0/resource/plugins/plugin-codex-relay/home` | 免鉴权 | 菜单入口，仅指向配置页 |

配置页会显示并允许修改：启用开关、`base_url`、`mode`（all/optin）、附加请求头，
以及已接管次数 / 跳过文件数等运行状态。

> **安全设计**：配置页与写接口都放在 `management` 路由下，走管理鉴权。
> CPA 会把「GET + 带 `Menu`」的路由降级为**免鉴权** resource 路由，因此菜单入口
> 单独注册且不渲染上游地址，避免敏感信息泄漏。

> **持久化**：界面保存只改内存，立即生效；重启后以 `config.yaml` 为准。
> 需要长期生效请同步改配置文件。

## 验证

部署后重启，日志应出现 `adopted` 而非 `skipped`：

```sh
docker logs cpa 2>&1 | grep 'plugin-codex-relay'
# [plugin-codex-relay] adopted file=codex-xxx.json base_url=http://172.19.0.1:8320/backend-api/codex
```

若全是 `unparsable`，通常是 ABI 的 `[]byte` 字段没按 base64 解码（见仓库 README 的坑）。

界面自检：

```sh
curl -s -H "Authorization: Bearer <management-key>" \
  http://127.0.0.1:8317/v0/management/codex-relay/status
```
