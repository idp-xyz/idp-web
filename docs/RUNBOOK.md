# idp-web 运维手册

适用：idp-web（Agent 搜索网页、读取网页，MCP 上游，放在 idp-mcp 网关后面）。设计与取舍见 [`adr/ADR-0001-web-design.md`](adr/ADR-0001-web-design.md)，为什么要有它、外泄通道怎么切断见平台的 ADR-0003。本手册讲 Kubernetes 部署（ADR-0001 D26）；整平台在 kind 上的验收见平台仓库的 `deploy/kubernetes/RUNBOOK.md`。

## 1. 它是什么，怎么接

- 一个 MCP 2026-07-28 服务器，端口 8580（`/mcp`），运维端口 9580（`/healthz`、`/readyz`、`/metrics`）。工具：`web_search`、`web_fetch`、`web_open_url`；经网关时带目标名前缀（`web__web_search`）。
- **一个副本**：编号 `[n]`、读取缓存和限流都是内存里的、按副本的（D19、D21、D22）。清单的策略是 `Recreate`，滚动重启时的几秒里没有服务，重启后编号从头开始（模型被告知再搜索一次）；`ref` 不受影响，只要密钥还在。不要把副本数改成 2。
- **只能放在网关后面**：它的网络策略只放网关和注册服务进来；每个 `tools/call` 还必须带网关签的调用者断言（D2），所以绕过网关、从别的 Pod 直连的请求没有身份，会被 403 拒绝。
- **它是唯一被允许出站到公网的组件**：命名空间 `idp-web` 里只有它。出口 NetworkPolicy 只放公网地址的 443 与 80，减去 `internal/addr` 拒绝的全部 IPv4 段（私网、回环、链路本地与云元数据、CGNAT、文档、基准测试、保留、组播）；服务自己也拒绝同一批地址，策略是集群对它的再做一遍。只有 IPv4：没有 IPv6 规则，就到不了任何 IPv6 地址。

## 2. 部署

```bash
# 1. 命名空间（带 restricted 与 idp-mcp/mcp-upstream 标签）
kubectl apply -f deploy/kubernetes/namespace.yaml
# 2. 密钥与公钥，都不在仓库里
kubectl -n idp-web create secret generic idp-web-ref-key --from-literal=ref.key="$(openssl rand -base64 48)"
kubectl -n idp-web create configmap idp-web-gateway-caller-keys --from-file=caller.pub=<网关的公钥 PEM>
kubectl -n idp-web create secret generic idp-web-search-key --from-literal=exa.key=<Exa 的 API 密钥>    # 或 brave.key
# 3. 选搜索服务（见 2.1），把镜像换成构建出的、按摘要固定的镜像，再应用
kubectl apply -k deploy/kubernetes
kubectl -n idp-web rollout status deploy/idp-web
```

`ref.key` 至少 32 字节，一行一把。`kubectl -n idp-web logs deploy/idp-web` 里 `idp-web listening` 之后，`/readyz` 才是 200（服务商、`ref` 密钥、租户文件都已载入）。

**镜像**：`bash deploy/build.sh` 在宿主上静态编译，再 `docker build --network none`（scratch，用户 65532），推到自己的 registry，把 `kustomization.yaml` 里的 `registry.example.com/idp/idp-web` 和 `sha256:000…` 换成真实值（按摘要，不用标签）。

### 2.1 选搜索服务（发布的清单里没有默认值）

清单里**没有** `IDP_WEB_PROVIDER`：选哪家就是决定谁看到 Agent 的每一个查询、在哪个地区、按谁的条款、什么价格，这是运维的决定。没设时服务启动即退出（状态码 2，日志写 `IDP_WEB_PROVIDER is not set`），Pod 不就绪：不会在生产里悄悄用 `fixture`。用一个 kustomize 补丁（或直接改 `web.yaml`）加：

```yaml
- name: IDP_WEB_PROVIDER
  value: exa                      # 或 brave
- name: IDP_WEB_EXA_KEY_FILE      # brave 用 IDP_WEB_BRAVE_KEY_FILE=/etc/idp/search/brave.key
  value: /etc/idp/search/exa.key
- name: IDP_WEB_EXA_BASE_URL      # 只用于 exa，只能是 https；欧盟端点 https://api.eu.exa.ai
  value: https://api.eu.exa.ai
```

`fixture` 只用于开发与验收，不要用在生产。**`IDP_WEB_TEST_ORIGINS` 永远不要设**：它让读取器免检某些主机与端口（D14），`tests/deploy` 保证发布的清单里没有它。

## 3. 在网关登记，并授权

idp-web 必须以 `identity: forward` 登记（注册后不可改），端点是 Service 的地址，并且**要与 `IDP_WEB_CALLER_AUDIENCE` 逐字相同**：`http://idp-web.idp-web.svc.cluster.local:8580/mcp`。网关签调用者断言时把这个端点写进断言，idp-web 只认为自己这个端点签的断言；差一个字符（多一个斜杠、`http` 写成 `https`、少了 `.svc.cluster.local`）每个 `tools/call` 都是 403。

```bash
# 注册服务（以 operator 的令牌）
POST /api/v1/servers  {"displayName":"web","targetName":"web","endpoint":"http://idp-web.idp-web.svc.cluster.local:8580/mcp","identity":"forward"}
POST /api/v1/servers/{id}/discover        # 应列出 web_search、web_fetch、web_open_url
POST /api/v1/servers/{id}/approve         # reviewer
POST /api/v1/grants  {"serverId":"{id}","roles":["<角色>"],"tools":["web_search","web_fetch","web_open_url"]}   # reviewer 批准
# 然后编译、批准、部署一个 Release
```

（platform 仓库的 `deploy/compose/mcp_setup.py` 是这一串调用的可运行版本。）注册服务的 `IDP_ALLOWED_UPSTREAM_HOSTS` 与 `IDP_PRIVATE_UPSTREAM_HOSTS` 要包含 `idp-web.idp-web.svc.cluster.local`；网关与注册服务到 `idp-web` 命名空间的 8580 出口，idp-mcp 的清单已经放行（标签 `idp-mcp/mcp-upstream=true`）。工具的 schema 或注解变了，网关钉住的目录会漂移，要重新 discover、评审、发布 Release。

**网关的签名**：网关设 `IDP_GATEWAY_CALLER_KEY_FILE`（P-256 私钥），公钥放进 ConfigMap `idp-web-gateway-caller-keys`。生成：`openssl ecparam -name prime256v1 -genkey -noout -out caller.key && openssl ec -in caller.key -pubout -out caller.pub`。两边的时钟差不要超过 30 秒。

**只把 web 工具授给"经 HOST 调用"的那个角色，不授给成员自己直接调网关的角色。** web 工具把内容送到企业之外，并读回不可信的页面文字；对它们的几条保护都在 HOST 里：`web__web_open_url` 一律要成员确认（HOST 配置的 `confirmTools`，不靠 Release），带 web 工具的运行不会拿到超过敏感度上限的上下文条目（`externalTools: ["web__*"]`），页面文字被标记为不可信。成员自己用令牌调网关，这些全都没有（idp-mcp 的互操作差距第 7 节；平台 ADR-0003 D7、D8）。所以：HOST 的配置里必须有

```yaml
gateways:
  - name: edge
    ...
    externalTools: ["web__*"]
    confirmTools: ["web__web_open_url"]
```

授权的角色只是 HOST 为运行换来的令牌带的那个角色（kind 验收里是 `host-user`）。

## 4. 租户文件

发布的租户文件是 `default: {enabled: false}`：没有列出的租户一律不可用。启用一个租户：改 `deploy/kubernetes/tenants.yaml`（键是网关转发的租户 id），再 `kubectl apply -k deploy/kubernetes`。

```yaml
default: {enabled: false}
tenants:
  7b0c0a52-…:
    enabled: true
    includeDomains: [example.com]          # 空或不写：所有公网域名；域名连子域名一起匹配
    excludeDomains: [private.example.com]  # 排除优先于包含
    openUrl: false                         # web_open_url：成员自己给地址；只有 true 才可用
```

- 服务只在启动时读一次。ConfigMap 由 kustomize 生成，名字随内容变，`apply` 之后 Pod 自动换。直接 `kubectl edit configmap` 不会让服务重读，要 `kubectl -n idp-web rollout restart deploy/idp-web`，而且 `apply` 会把手工改动覆盖回去：以文件为准。
- 未知的键、不是主机名的域名项（带协议、路径、端口、通配符）是错误，服务启动即退出（2）：看日志，不要猜。
- 启用 `openUrl` 之前确认 HOST 的 `confirmTools` 已配好（第 3 节）；否则一个被页面里的指令骗了的模型能让服务读任何公网地址。
- 域名名单传给搜索服务商，并由 idp-web 在搜索结果和读取（每次重定向）时再强制一次。

## 5. 轮换

- **`ref` 密钥**：把新密钥写在 Secret 的**第一行**，旧的留在后面（第一把签发，全部验证），重启（`rollout restart`）。`ref` 的寿命一小时，一小时后删掉旧的一行，再重启。直接换掉 Secret 而不留旧行，在那一小时内已发出的 `ref` 全部失效（模型会得到 `ref_malformed`，重新搜索即可，不是故障）。
- **网关的签名密钥**：先把新公钥**追加**进 `idp-web-gateway-caller-keys`（`caller.pub` 里可以放几个 PEM 块）并重启 idp-web，再换网关的私钥，最后删掉旧公钥再重启一次。顺序反了，换密钥的那一段时间里所有调用都是 403。
- **搜索服务的密钥**：更新 Secret，重启。

## 6. 日志与指标

- **审计**：标准输出上的 JSON，每次工具调用一行 `"msg":"audit"`，字段：`tool`、`outcome`、`ms`、`tenant`、`caller`（租户与主体的短哈希，不记成员标识本身）、`query` 与 `url`（明文：它们就是外发的内容，包括被拒绝的地址）、`results`、`dropped`、`bytes`、`cache`、`decision`（`allowed`、`refused`、`failed`）、`reason`。**没有 `ref`、密钥、页面文字**（D23）。`kubectl -n idp-web logs deploy/idp-web | grep '"msg":"audit"'`。留存期是集群日志策略的事，不在本服务里。
- **指标**：`idp_web_calls_total{tool,outcome}`、`idp_web_refused_total{tool,reason}`、`idp_web_call_seconds_total{tool}`，在 9580，只放 `observability` 命名空间抓取。
- **拒绝原因**（审计里的 `reason`，指标里的 `reason`）：

| `reason` | `decision` | 含义与处理 |
|---|---|---|
| `ok` | `allowed` | 成功 |
| `ref_malformed`、`ref_mismatch` | `refused` | `web_fetch` 的地址与 `ref` 对不上：地址不是搜索结果里的（模型自己拼的，或页面里的指令让它去读的）。**这是设计，不是故障**；大量出现说明有页面在诱导模型。`ref` 密钥换过也会出现 `ref_malformed` |
| `ref_expired` | `refused` | `ref` 超过一小时：重新搜索 |
| `tenant_disabled` | `refused` | 租户没有启用：第 4 节 |
| `open_url_disabled` | `refused` | 租户 `openUrl` 为 false |
| `domain_excluded`、`domain_not_included` | `refused` | 租户的域名名单 |
| `address_refused`、`address_not_public` | `refused` | 地址不是普通的公网 http(s) 地址（私网、回环、元数据、带用户名密码、非规范写法），或域名解析到了不公开的地址 |
| `robots` | `refused` | 站点的 robots.txt 不许读 |
| `rate_limited` | `refused` | 调用者超过窗口内的搜索或读取次数（`IDP_WEB_LIMIT_*`） |
| `invalid_arguments`、`no_caller` | `refused` | 参数不合 schema；请求没带调用者（没经过网关） |
| `provider_busy`、`provider_error` | `failed` | 搜索服务限流或出错：看服务商的状态与密钥；日志里有详情（`search provider request failed` 或 `search provider answered with an error`，带状态码） |
| `too_large`、`too_slow`、`content_type`、`pdf`、`encoding`、`bot_check`、`http_status`、`redirects`、`connect`、`unreadable` | `failed` | 网站没给出可读的内容（太大、太慢、不是文字、是 PDF、编码不认识、要人机验证、非 2xx、重定向太多、连不上、提取不出文字） |
| `unavailable`、`internal`、`canceled` | `failed` | 服务自己的问题或调用被取消：日志里 `tool failed` 有详情 |

## 7. 故障排查

| 现象 | 原因 | 处理 |
|---|---|---|
| Pod 一直 `ContainerCreating`，事件里 `secret "idp-web-ref-key" not found` / `configmap "idp-web-gateway-caller-keys" not found` | 第 2 节的第 2 步没做 | 建 Secret 与 ConfigMap |
| Pod `CrashLoopBackOff`，日志 `IDP_WEB_PROVIDER is not set` | 没选搜索服务（设计如此） | 第 2.1 节 |
| 日志 `IDP_WEB_EXA_KEY_FILE: …` 或 `the file is empty` | 密钥 Secret 缺、键名不是 `exa.key`、内容为空 | 对一下 Secret 的键名与路径 |
| 日志 `IDP_WEB_TENANTS_FILE: … field … not found` | 租户文件里有未知的键 | 改文件，`apply` |
| 每个 `tools/call` 都是 403 `the caller assertion is missing or invalid` | 没有网关的签名，或签得不对：网关没设 `IDP_GATEWAY_CALLER_KEY_FILE`、公钥不是那把私钥的、登记的端点与 `IDP_WEB_CALLER_AUDIENCE` 不逐字相同、时钟差超过 30 秒，或有人绕过网关直连 | 看日志里 `caller assertion refused` 的 `reason`；对一下网关启动日志里的 `kid` 与 idp-web 启动日志里的 `keys` |
| 登记时 discover 超时 | 网络策略：网关与注册服务到 `idp-web` 的 8580 | `idp-web` 命名空间要带 `idp-mcp/mcp-upstream: "true"`；`kubectl -n idp-web get networkpolicy`；注册服务的 `IDP_ALLOWED_UPSTREAM_HOSTS` 要含这个主机名 |
| 网关到 idp-web 超时（调用时，不是登记时） | 同上，或 Pod 不就绪 | `kubectl -n idp-web get pods`，`/readyz` |
| `web_search` 全是 `provider_error` | 服务商的密钥错或额度用完；或出口策略把服务商的地址挡了（集群的 Pod、Service、节点网段落在 `except` 之外但服务商在 `except` 的某段内，一般不会）| 看日志里 `search provider …` 的警告（状态码与服务商的回答）；镜像是 scratch，没有 shell，进不去，看日志与 `/metrics` |
| 所有读取 `too_slow` 或 `connect`，搜索正常 | 出口策略没放行 80 / 443（被策略丢弃的连接等不到响应，表现为 `too_slow`），或 CNI 不执行 NetworkPolicy 之外的东西挡了 | 确认 CNI 执行策略；节点是否能出站 |
| 读内网的页面一律被拒 | 这是设计：发布的出口规则和服务都排除私网地址。验收里的测试源站靠测试专用的策略与 `IDP_WEB_TEST_ORIGINS`，生产里不要照做 | 内部资料不走这个组件 |
| `ref_malformed` 突然很多 | `ref` 密钥换了而没留旧行，或服务重启后换了密钥 | 第 5 节 |
| 编号 `[n]` 在重启后从 1 开始 | 编号在内存里、按副本（D19） | 预期 |

## 8. 安全边界（运维要知道的）

- 网络策略加服务自己的地址规则是两道独立的拒绝：不要为了"让它读内网"去放宽任何一道。
- `openUrl`、域名名单、搜索服务商是三个影响数据外流的开关，改它们要有评审。
- 阶段 1 的差距：每次运行的调用上限做不到（调用者断言里没有运行号），按调用者限流，每次运行的上限由 HOST 的步数限制兜底；编号、限流与缓存在内存里，按副本（ADR-0001 D19、D22）。
- 只在 kind + Calico 上由平台验收测过（PATH M 与 N，页面是集群里的测试源站）；没有真实的公网流量走过 Kubernetes 上的出口规则，也没有在真实的搜索服务商上跑过。
