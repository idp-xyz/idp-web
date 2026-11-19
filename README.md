# idp-web

IDP TELOS 的联网组件：一个 MCP 服务器，让 Agent 搜索网页、读取网页内容。它放在 idp-mcp 网关后面作为上游：认证、授权、参数校验和调用审计都是网关的事；idp-web 只接受网关签了名的调用者。整体设计见平台仓库的 ADR-0003（网页搜索与读取），本仓库自己的决策见 [`docs/adr/ADR-0001-web-design.md`](docs/adr/ADR-0001-web-design.md)。

状态：**骨架**。MCP 服务、调用者校验、三个工具的最终声明（参数与标注）、配置、健康检查、指标、镜像和 CI 已经有；**三个工具还没有实现**：每次调用返回"web search is not available yet"。搜索适配器、`ref`、读取与正文提取是后续任务。

## 工具

| 工具 | 参数 | 说明 |
|---|---|---|
| `web_search` | `query`、可选 `freshness`（`day`、`week`、`month`、`year`）、可选 `count`（1 到 10） | 搜索 |
| `web_fetch` | `url`、`ref`、可选 `offset` | 读取搜索结果里出现过的地址，`ref` 是该地址的凭证 |
| `web_open_url` | `url`、可选 `offset` | 读取成员给的地址；确认由 HOST 强制 |

三个工具都是 `readOnlyHint: true`、`openWorldHint: true`，`additionalProperties: false`。租户和成员只取自网关签名的调用者，不是参数。经网关时名字带上目标名前缀（`web__web_search`）。

## 使用

```bash
make test               # gofmt、vet、单元测试（-race）
make build              # bin/idp-web
bash deploy/build.sh    # 镜像（scratch，用户 65532）：宿主上静态编译，再 docker build --network none
```

运行：`IDP_WEB_LISTEN=127.0.0.1:8580 ./bin/idp-web`（配置项见 `cmd/idp-web/main.go` 文件头）。绑定非回环地址必须设置 `IDP_WEB_PRIVATE_NETWORK=true`：服务器自己没有认证，只有网关能到达它。

## 配置

环境变量；配置有错时写到标准错误并以状态码 2 退出。

| 变量 | 缺省 | 说明 |
|---|---|---|
| `IDP_WEB_LISTEN` | `127.0.0.1:8580` | `/mcp` 的地址；非回环地址要同时设 `IDP_WEB_PRIVATE_NETWORK=true` |
| `IDP_WEB_PRIVATE_NETWORK` | 空 | 运维的声明：只有网关能到达本服务 |
| `IDP_WEB_METRICS` | `127.0.0.1:9580` | `/healthz`、`/readyz`、`/metrics`（`idp_web_calls_total{tool,outcome}`、`idp_web_call_seconds_total{tool}`） |
| `IDP_WEB_CALLER_KEYS_FILE` | 空 | 网关的公钥（PEM，P-256，可放几把）；设了之后每个 `tools/call` 必须带网关的签名断言 |
| `IDP_WEB_CALLER_AUDIENCE` | 空 | 本服务在 idp-mcp 里登记的端点，如 `http://web:8580/mcp`；与上一项要么都设、要么都不设 |
| `IDP_WEB_REQUIRE_CALLER` | `true` | 请求必须带调用者；`false` 只用于单机开发 |

日志是标准输出上的 JSON（slog）；每次工具调用一行：工具、结果、耗时、调用者的短哈希（不记成员标识本身）。

## 许可证

[PolyForm Noncommercial License 1.0.0](LICENSE)。
