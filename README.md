# idp-web

IDP TELOS 的联网组件：一个 MCP 服务器，让 Agent 搜索网页、读取网页内容。它放在 idp-mcp 网关后面作为上游：认证、授权、参数校验和调用审计都是网关的事；idp-web 只接受网关签了名的调用者。整体设计见平台仓库的 ADR-0003（网页搜索与读取），本仓库自己的决策见 [`docs/adr/ADR-0001-web-design.md`](docs/adr/ADR-0001-web-design.md)。

状态：**三个工具可用**。MCP 服务、调用者校验、地址检查与读取、正文提取、搜索适配器（`exa`、`brave`、`fixture`）、签名的 `ref`、租户文件、限流、读取缓存、审计、指标、镜像、CI 和 Kubernetes 清单都已具备。设计决策见 ADR-0001（D1 到 D26）。运维见 [`docs/RUNBOOK.md`](docs/RUNBOOK.md)。

## 工具

| 工具 | 参数 | 说明 |
|---|---|---|
| `web_search` | `query`、可选 `freshness`（`day`、`week`、`month`、`year`）、可选 `count`（1 到 10） | 搜索 |
| `web_fetch` | `url`、`ref`、可选 `offset` | 读取搜索结果里出现过的地址，`ref` 是该地址的凭证；地址与 `ref` 对不上就拒绝，所以模型自己拼的地址读不了 |
| `web_open_url` | `url`、可选 `offset` | 读取成员给的地址；只有租户文件里 `openUrl: true` 的租户可用，确认由 HOST 强制 |

`web_search` 返回编号的结果（`[n] 标题`、`address:`、`ref:`、`published:`、`excerpt:`）和 `structuredContent.citations`（`n`、`title`、`url`）；`web_fetch` 与 `web_open_url` 返回正文的一段（12 KiB），首行写明总长度与位置，`citations` 里有最终地址、读取时间和这一段的 SHA-256。同一个调用者看到的同一个地址总是同一个编号 `n`。

三个工具都是 `readOnlyHint: true`、`openWorldHint: true`，`additionalProperties: false`。租户和成员只取自网关签名的调用者，不是参数。经网关时名字带上目标名前缀（`web__web_search`）。

## 使用

```bash
make test               # gofmt、vet、单元测试（-race）
make build              # bin/idp-web
bash deploy/build.sh    # 镜像（scratch，用户 65532）：宿主上静态编译，再 docker build --network none
```

CI 里的第三方镜像（扫描用的 trivy）先由 `deploy/pull-image.sh` 显式拉取并重试（最多 5 次，间隔 15、30、60、120 秒；本机已有则跳过），因为公共 registry 对匿名拉取按来源地址限流，而 CI 的 runner 与许多项目共用地址。

运行：见下面"本机用 fixture 运行"。配置项见 `cmd/idp-web/main.go` 文件头与下表。绑定非回环地址必须设置 `IDP_WEB_PRIVATE_NETWORK=true`：服务器自己没有认证，只有网关能到达它。

## 在 Kubernetes 上

`deploy/kubernetes`（命名空间 `idp-web`，一个副本，restricted，默认拒绝的 NetworkPolicy，出口只到公网地址的 443 与 80）：`kubectl apply -k deploy/kubernetes`，之前要先建 `ref` 密钥、网关公钥和搜索服务的密钥，并选搜索服务，步骤见 [`docs/RUNBOOK.md`](docs/RUNBOOK.md)。发布的租户文件里谁也不能用，发布的清单里没有搜索服务的默认值（没选就不启动）。在 idp-mcp 里登记的端点，也就是 `IDP_WEB_CALLER_AUDIENCE`，在 Kubernetes 上是 Service 的地址：`http://idp-web.idp-web.svc.cluster.local:8580/mcp`。清单由 `tests/deploy` 检查（与代码一致、没有测试专用的东西、出口规则与 `internal/addr` 一致），整平台的验收是 idp-telos-go 的 `tests/kind/full.sh`（PATH M 与 N）。

## 配置

环境变量；配置有错时写到标准错误并以状态码 2 退出。

| 变量 | 缺省 | 说明 |
|---|---|---|
| `IDP_WEB_LISTEN` | `127.0.0.1:8580` | `/mcp` 的地址；非回环地址要同时设 `IDP_WEB_PRIVATE_NETWORK=true` |
| `IDP_WEB_PRIVATE_NETWORK` | 空 | 运维的声明：只有网关能到达本服务 |
| `IDP_WEB_METRICS` | `127.0.0.1:9580` | `/healthz`、`/readyz`（服务商、`ref` 密钥、租户文件都载入了才是 200）、`/metrics`（`idp_web_calls_total{tool,outcome}`、`idp_web_refused_total{tool,reason}`、`idp_web_call_seconds_total{tool}`） |
| `IDP_WEB_CALLER_KEYS_FILE` | 空 | 网关的公钥（PEM，P-256，可放几把）；设了之后每个 `tools/call` 必须带网关的签名断言 |
| `IDP_WEB_CALLER_AUDIENCE` | 空 | 本服务在 idp-mcp 里登记的端点，如 `http://web:8580/mcp`；与上一项要么都设、要么都不设 |
| `IDP_WEB_TEST_ORIGINS` | 空 | 只用于测试：免检的 `主机:端口`（ADR-0001 D14） |
| `IDP_WEB_REQUIRE_CALLER` | `true` | 请求必须带调用者；`false` 只用于单机开发 |
| `IDP_WEB_PROVIDER` | **必填** | 搜索服务：`fixture`、`exa`、`brave`；没设，或它的密钥或文件缺失，启动即退出（2） |
| `IDP_WEB_EXA_KEY_FILE` | 空 | `exa` 的 API 密钥文件 |
| `IDP_WEB_EXA_BASE_URL` | `https://api.exa.ai` | Exa 的地址，必须是 https；欧盟端点 `https://api.eu.exa.ai` |
| `IDP_WEB_EXA_TYPE` | `auto` | `auto` 或 `lite` |
| `IDP_WEB_BRAVE_KEY_FILE` | 空 | `brave` 的 API 密钥文件 |
| `IDP_WEB_FIXTURE_FILE` | 空 | `fixture` 的结果文件（YAML），开发与验收用，不联网 |
| `IDP_WEB_REF_KEY_FILE` | **必填** | `ref` 的密钥，一行一把，每把至少 32 字节；第一把签发，全部验证（轮换：新密钥放前面） |
| `IDP_WEB_TENANTS_FILE` | **必填** | 租户文件（YAML），启动时读一次 |
| `IDP_WEB_LIMIT_SEARCHES` | `30` | 每个调用者在窗口内的搜索次数 |
| `IDP_WEB_LIMIT_READS` | `30` | 每个调用者在窗口内真正发往网站的读取次数（缓存命中的后几段不算） |
| `IDP_WEB_LIMIT_WINDOW_S` | `600` | 窗口，秒 |

密钥一律从文件读，不从环境变量读。

### 租户文件

```yaml
default: {enabled: false}          # 没有列出的租户用它；没有 default 时一律不可用
tenants:
  acme:
    enabled: true
    includeDomains: [example.com]  # 可空：空表示所有域名；域名项连子域名一起匹配
    excludeDomains: [private.example.com]   # 排除优先于包含
    openUrl: false                 # web_open_url，缺省为关
  labs: {enabled: true, openUrl: true}
```

未知的键是错误。域名名单在搜索时传给服务商，并由 idp-web 自己在搜索结果和读取（包括每次重定向）时再强制一次。

### fixture 文件

```yaml
entries:
  - match: parcel                  # 查询词里含这个子串（不分大小写）就用这一条
    results:
      - {url: "http://127.0.0.1:8099/article.html", title: "The Parcel Report", published: "2026-11-01", snippet: "The depot sorted forty thousand parcels"}
default: []                        # 可选：没有一条匹配时的结果
```

## 本机用 fixture 运行

```bash
mkdir -p /tmp/idpweb && cd /tmp/idpweb
head -c 40 /dev/urandom | base64 -w0 > ref.key                  # 至少 32 字节
printf 'default: {enabled: false}\ntenants:\n  acme: {enabled: true}\n' > tenants.yaml
# fixture.yaml 如上，指向本机的测试源站；源站：python3 -m http.server 8099 --bind 127.0.0.1
IDP_WEB_PROVIDER=fixture IDP_WEB_FIXTURE_FILE=fixture.yaml IDP_WEB_REF_KEY_FILE=ref.key \
IDP_WEB_TENANTS_FILE=tenants.yaml IDP_WEB_TEST_ORIGINS=127.0.0.1:8099 ./bin/idp-web
curl -s localhost:9580/readyz        # ready
```

没有设置调用者公钥时，`_meta["io.idp.telos/caller"]` 就是调用者（只有网络在担保，仅限开发）：

```bash
curl -s localhost:8580/mcp -H 'MCP-Protocol-Version: 2026-07-28' -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"parcel news"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.idp.telos/caller":{"subject":"alice","tenant":"acme","roles":["member"]}}}}'
```

再用结果里的地址与 `ref` 调 `web_fetch`。

## 日志与指标

日志是标准输出上的 JSON（slog）；每次工具调用一行 `"msg":"audit"`：工具、结果、耗时、租户、调用者的短哈希（不记成员标识本身）、查询词与地址（明文，它们就是外发的内容）、返回与丢弃的结果数、字节数、决定与原因。**没有 `ref`、密钥和页面文字**（ADR-0001 D23）。

## 已知差距

每次运行的调用上限做不到（调用者断言里没有运行号）：阶段 1 按调用者限流，每次运行的上限由 HOST 的步数限制兜底。编号、限流与缓存在内存里，按副本：阶段 1 只跑一个副本，阶段 2 把编号移到 HOST 的 `sources`。

## 许可证

[PolyForm Noncommercial License 1.0.0](LICENSE)。
