# idp-web 运维手册

适用：idp-web（Agent 搜索网页、读取网页，MCP 上游，放在 idp-mcp 网关后面）。设计与取舍见 [`adr/ADR-0001-web-design.md`](adr/ADR-0001-web-design.md)，为什么要有它、外泄通道怎么切断见平台的 ADR-0003。本手册讲 Kubernetes 部署（ADR-0001 D26）；整平台在 kind 上的验收见平台仓库的 `deploy/kubernetes/RUNBOOK.md`。

## 1. 它是什么，怎么接

- 一个 MCP 2026-07-28 服务器，端口 8580（`/mcp`），运维端口 9580（`/healthz`、`/readyz`、`/metrics`）。工具：`web_search`、`web_fetch`、`web_open_url`；经网关时带目标名前缀（`web__web_search`）。
- **副本数：配置了数据库就可以多于一个**（第 9 节）。每日配额与来源编号 `[n]` 在 PostgreSQL 里，所有副本一致、重启之后不变（D28、D29）；读取缓存与滑动窗口限流仍是各副本的（D21、D22）：N 个副本时一个调用者在窗口内最多可做 N 倍的调用，每日配额才是精确的上限。清单的策略是 `RollingUpdate`，`replicas` 写 1，由你调高。**不配数据库（清单之外的部署）则必须是一个副本、策略 `Recreate`**：编号在内存里，两个 Pod 会给同一调用者两套编号；这样的部署也不能在租户文件里写每日限额。`ref` 不受影响，只要密钥在所有副本上一致。
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
# 3. 数据库：Secret 与第一次迁移（第 9 节：建库、建角色、两个 Secret）
# 4. 选搜索服务（见 2.1），把镜像换成构建出的、按摘要固定的镜像，再应用
kubectl apply -k deploy/kubernetes          # 含迁移的 Job idp-web-migrate；Deployment 在库迁好之前起不来，会重启到迁好为止
kubectl -n idp-web wait --for=condition=complete job/idp-web-migrate --timeout=120s
kubectl -n idp-web rollout status deploy/idp-web
```

`ref.key` 至少 32 字节，一行一把。`kubectl -n idp-web logs deploy/idp-web` 里 `idp-web listening` 之后，`/readyz` 才是 200（服务商、`ref` 密钥、租户文件都已载入，数据库在应答）。

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
    dailySearches: 200                     # 每个 UTC 日这个租户最多搜索几次：不写＝不限；0＝一次都不允许
    dailyReads: 600                        # 每个 UTC 日真正发往网站的读取（缓存命中的后几段不算）
```

- **每日配额**（`dailySearches`、`dailyReads`，D28）：按租户、按 UTC 日（00:00 UTC 清零），在 PostgreSQL 里数，所有副本共用；搜索在调用服务商时算（服务商答错也算），读取在真正发往网站时算（缓存命中、被地址规则或名单在发出前拒绝的、解析到内部地址而没有连接的，不算）。用完时模型读到"今天的额度用完了，00:00 UTC 恢复，用已有的内容回答"，审计 `reason=quota_exceeded`。写了任何一个限额就必须配置数据库，否则服务启动即退出（2）。租户条目整体替换 `default`，不合并。
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
- **指标**：`idp_web_calls_total{tool,outcome}`、`idp_web_refused_total{tool,reason}`、`idp_web_failed_total{tool,reason}`（网站或服务商没给出的调用，按原因）、`idp_web_call_seconds_total{tool}`，在 9580，只放 `observability` 命名空间抓取。
- **拒绝原因**（审计里的 `reason`，指标里的 `reason`）：

| `reason` | `decision` | 含义与处理 |
|---|---|---|
| `ok` | `allowed` | 成功 |
| `ref_malformed`、`ref_mismatch` | `refused` | `web_fetch` 的地址与 `ref` 对不上：地址不是搜索结果里的（模型自己拼的，或页面里的指令让它去读的）。**这是设计，不是故障**；大量出现说明有页面在诱导模型。`ref` 密钥换过也会出现 `ref_malformed` |
| `ref_expired` | `refused` | `ref` 超过一小时：重新搜索 |
| `tenant_disabled` | `refused` | 租户没有启用：第 4 节 |
| `open_url_disabled` | `refused` | 租户 `openUrl` 为 false |
| `domain_excluded`、`domain_not_included` | `refused` | 租户的域名名单 |
| `address_refused` | `refused` | 地址的写法不被接受：不是 http 或 https、端口不是 80 或 443、带用户名密码、非规范的 IP 写法（`2130706433`、`0x7f.1`）、格式错误、过长。还没有碰到地址指向哪里 |
| `address_not_public` | `refused` | 地址指向内部：IP 字面量不是公网地址（私网、回环、链路本地与元数据、CGNAT、多播、内嵌 IPv4 的 IPv6），或域名解析到了这样的地址，或重定向去了这样的地址。大量出现说明有页面或模型在试探内网（此前这一类也记为 `address_refused`，2026-11-19 起分开） |
| `robots` | `refused` | 站点的 robots.txt 不许读 |
| `rate_limited` | `refused` | 调用者超过窗口内的搜索或读取次数（`IDP_WEB_LIMIT_*`）；窗口按副本数 |
| `quota_exceeded` | `refused` | 租户今天（UTC）的每日限额用完了（`dailySearches` 或 `dailyReads`，包括限额为 0）；`idp_web_refused_total{reason="quota_exceeded"}` 看用量，第 9 节看怎么查表 |
| `quota_unavailable` | `refused` | 查不了每日限额（数据库超时、连不上，或没有配置数据库而租户有限额）：调用**没有发出**，也没有计数；日志里有 `the daily quota could not be checked` 与原因。第 9 节 |
| `numbering_unavailable` | `failed` | 数据库没给出来源编号：配置了数据库时，没有设限额的租户也依赖它；页面已读到的话已进缓存，数据库恢复后重试不会再请求网站 |
| `invalid_arguments`、`no_caller` | `refused` | 参数不合 schema；请求没带调用者（没经过网关） |
| `provider_busy`、`provider_error` | `failed` | 搜索服务限流或出错：看服务商的状态与密钥；日志里有详情（`search provider request failed` 或 `search provider answered with an error`，带状态码） |
| `too_large`、`too_slow`、`content_type`、`encoding`、`bot_check`、`http_status`、`redirects`、`connect`、`unreadable` | `failed` | 网站没给出可读的内容（太大、太慢、不是文字也不是 PDF、编码不认识、要人机验证、非 2xx、重定向太多、连不上、提取不出文字） |
| `pdf_no_text_layer` | `failed` | PDF 没有文字层（扫描件、只有图；每页不足 20 个字符的页不算有文字，有文字的页不到十分之一就是没有）：模型读到"换一个来源，或用搜索结果的摘录"。这不是故障，没有 OCR（D31） |
| `pdf_encrypted`、`pdf_malformed`、`pdf_too_large` | `failed` | PDF 要用户密码；损坏或根本不是 PDF（包括标成 PDF 而内容不是的）；解压后的数据过多（压缩炸弹）或读取程序超出内存上限而中止。日志里 `the PDF reader refused the document`（带读取程序的原因词与细节）或 `the PDF reader died`（带退出状态与它的第一行错误输出） |
| `pdf_too_slow` | `failed` | PDF 读取在期限（6 秒，且不超过这次调用剩下的时间）内没完成，进程组已被杀掉。日志 `the PDF reader was stopped at its deadline` |
| `pdf_busy` | `failed` | 同时读取的 PDF 数达到 `IDP_WEB_PDF_CONCURRENCY`，等了 2 秒仍没有空位：重试即可；持续出现就调大并发或副本数（每个进程最多 192 MiB） |
| `pdf_reader_unavailable` | `failed` | PDF 读取不可用：`IDP_WEB_PDF=off`，或启动时没找到 `idp-web-pdf`、它起不来或没通过自检（日志 `reading PDFs is not available: …` 写明原因，启动时一行，之后每次 PDF 调用都是这个原因）。镜像里应有 `/idp-web-pdf`（第 6.1 节） |
| `needs_script` | `failed` | 页面的正文由脚本生成，只读 HTML 看不到（框架页：空的挂载点、要求开启 JavaScript 的提示、正文在脚本字符串里），文字又很少；模型读到"换一个来源，或用搜索结果的摘录"。这类调用占 `web_fetch` 的份额（`idp_web_failed_total{tool="web_fetch",reason="needs_script"}` 除以 `idp_web_calls_total{tool="web_fetch"}`）是平台 ADR-0003 附录 D 第 6 节决定渲染器做不做的数据，门槛 5%；它是下限（见 ADR-0001 D27 的"不识别的"）。日志里 `the page needs script` 一行带规则名（`empty_mount`、`placeholder_mount`、`skeleton`、`noscript_notice`、`script_prose`）。验证页仍是 `bot_check`，不是它 |
| `unavailable`、`internal`、`canceled` | `failed` | 服务自己的问题或调用被取消：日志里 `tool failed` 有详情 |

### 6.1 PDF 读取

- **镜像里有两个文件**：`/idp-web`（服务）与 `/idp-web-pdf`（PDF 读取程序，每份 PDF 起一个进程，自己给自己设限制后才读文件）。`deploy/build.sh` 两个都编；用别的办法造镜像时别漏掉第二个，否则启动日志是 `reading PDFs is not available: the PDF reader command was not found`，服务照常运行，PDF 一律回答不可用。
- **关掉**：`IDP_WEB_PDF=off`（立即生效需要重启 Pod）。关掉后 PDF 仍会被下载（受 5 MiB、15 秒的限制）再回答"不可用"；要连下载也不做，就用域名名单。
- **限制**（D31）：下载 5 MiB 与 15 秒不变；读取最多 6 秒、最多 192 MiB 内存（`RLIMIT_DATA`）、最多 200 页；读取程序没有网络、不能起别的程序、没有环境变量、没有继承的文件描述符。同时最多 `IDP_WEB_PDF_CONCURRENCY`（2）个，Pod 的内存上限要容得下（清单里是 1 GiB）。
- **要求**：Linux amd64 或 arm64，内核 5.9 以上最好（旧内核上关闭继承描述符退化为逐个关）；容器的 seccomp 档案要允许 `seccomp` 与 `prctl` 系统调用（Docker 与 containerd 的缺省档案都允许）。限制施加不了时读取程序拒绝运行，启动自检失败，服务报告 PDF 不可用，不会无限制地解析。
- **看一份 PDF 为什么读不了**：`docker run --rm -i --entrypoint /idp-web-pdf <镜像> < 文件.pdf`，标准错误第一行是原因词与细节，退出码非零；成功时标准输出是文本，标准错误一行 `pdf pages=… read=… title=…`。输出不能重定向到文件（`> out.txt`）：读取程序的文件大小限制是 0，这是有意的（它不能写文件），所以标准输出或标准错误是普通文件时它直接以 `unavailable: standard output is a file…` 与退出码 2 结束，要存文件就接管道（`… | cat > out.txt`）。

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
| 个别站点的读取偶尔 `too_slow`（如 www.fedex.com 从没有可用 IPv6 的网络），IPv4 用 curl -4 很快 | 该网络的 IPv6 连得上但随后不通：连接竞速（D32）看不出来。默认 `IDP_WEB_DIAL_FAMILY=prefer-ipv4` 已让 IPv4 先拨；若设成了 `dual` 或 `ipv6-only` 就改回。纯 IPv6 的网络用 `dual`（默认值在那里多花 250 ms） | ADR-0001 D32 |
| 读内网的页面一律被拒 | 这是设计：发布的出口规则和服务都排除私网地址。验收里的测试源站靠测试专用的策略与 `IDP_WEB_TEST_ORIGINS`，生产里不要照做 | 内部资料不走这个组件 |
| `ref_malformed` 突然很多 | `ref` 密钥换了而没留旧行，或服务重启后换了密钥 | 第 5 节 |
| 所有 PDF 都回答"reading PDFs is not available" | 启动日志里有原因：没设对 `IDP_WEB_PDF`、镜像里没有 `/idp-web-pdf`、自检没通过（限制施加不了：seccomp 档案太严、内核太旧、不是 amd64 或 arm64） | 第 6.1 节；改好后重启 |
| 日志 `the PDF reader died` 很多 | 读取程序在限制下中止：内存上限（`fatal error: runtime: out of memory`）或崩溃；每次只影响那一次调用 | 看第一行错误输出；个别文档预期如此，成批出现则是解析库的问题，保留文档报告 |
| 编号 `[n]` 在重启后从 1 开始 | 没有配置数据库：编号在内存里、按副本（D19）；配置了数据库则不会，除非那个调用者已经一小时没用 | 预期 / 第 9 节 |
| 日志 `IDP_WEB_TENANTS_FILE: … set dailySearches or dailyReads, which needs the database` | 租户文件里有限额而没有配置数据库 | 设 `IDP_WEB_DATABASE_URL_FILE`，或去掉限额 |
| 日志 `schema at version 0, binary needs 1 (run idp-web migrate)` / `schema not migrated` | 迁移没跑，或镜像比库新 | 先跑 Job `idp-web-migrate`，再滚动 Deployment |
| 日志 `refusing to run as a superuser or BYPASSRLS role` | 服务的 URL 用的是所有者或超级用户 | 用 `ensure-login` 建的登录角色 |
| `/readyz` 503 `database not reachable` | 数据库不应答（超时 `IDP_WEB_DATABASE_TIMEOUT_MS`），或出口策略 `web-database` 的选择器与你的 PostgreSQL 不符 | 第 9 节；有限额的租户此时的调用被拒绝，其余租户的搜索与读取也做不了 |

## 8. 安全边界（运维要知道的）

- 网络策略加服务自己的地址规则是两道独立的拒绝：不要为了"让它读内网"去放宽任何一道。
- `openUrl`、域名名单、搜索服务商是三个影响数据外流的开关，改它们要有评审。
- 差距：每次运行的调用上限做不到（调用者断言里没有运行号），按调用者限流，每次运行的上限由 HOST 的步数限制兜底；滑动窗口与缓存在内存里、按副本（D21、D22），编号与每日配额在数据库里（D28、D29）。
- 只在 kind + Calico 上由平台验收测过（PATH M 与 N，页面是集群里的测试源站）；没有真实的公网流量走过 Kubernetes 上的出口规则，也没有在真实的搜索服务商上跑过。

## 9. 数据库、每日配额与副本

idp-web 用 PostgreSQL 保存两样东西：每个租户每个 UTC 日的搜索与读取次数（每日配额，D28），和每个调用者看到的来源编号（D29）。表里只有计数、号码和调用者与地址的 SHA-256；**没有查询词、地址、页面、调用者的主体**。没有配置数据库时（清单之外的部署）编号在内存里、只能单副本，租户文件里不能写每日限额。

**建库与角色**（一次；与 idp-mcp、idp-host 同一套做法）。数据库 `idp_web` 由 DBA 建，所有者角色（建表的）的 URL 放进 Secret `idp-web-database-owner` 的 `owner-url`，只有 Job 挂它。然后：

```bash
kubectl -n idp-web create secret generic idp-web-database-owner --from-literal=owner-url='postgres://idp_web_owner:<密码>@postgresql.databases.svc.cluster.local:5432/idp_web?sslmode=verify-full'
kubectl apply -k deploy/kubernetes          # Job idp-web-migrate 建表与角色 idp_web_app；这时 Deployment 还起不来（没有服务角色的 Secret），这是预期
# 建服务用的登录角色（idp_web_app 的成员，不是超级用户）：用同一个镜像，一次性运行
kubectl -n idp-web run idp-web-ensure-login --rm -it --restart=Never --image=<镜像> \
  --env=IDP_WEB_MIGRATION_DATABASE_URL=<所有者的 URL> --env=IDP_WEB_APP_DB_PASSWORD=<服务角色的密码> \
  --command -- /idp-web ensure-login idp_web_svc
kubectl -n idp-web create secret generic idp-web-database \
  --from-literal=service-url='postgres://idp_web_svc:<密码>@postgresql.databases.svc.cluster.local:5432/idp_web?sslmode=verify-full&sslrootcert=/etc/idp/database/ca.crt' \
  --from-file=ca.crt=<签发数据库服务器证书的 CA>
```

（那条一次性命令把密码放在命令行里，只在受控的终端上做，或改用一个临时的、挂 Secret 的 Pod。）**TLS**：选项都在 URL 里：`sslmode=verify-full`（服务器证书必须由 `sslrootcert` 指的 CA 签发，且名字要对）；镜像没有系统 CA，所以私有 CA 必须用 `sslrootcert`，公共 CA 由内置的 Mozilla 根兜底。`sslmode=disable` 只用于本机测试。

**升级**：先跑迁移的 Job（它只往前走；服务在库的版本与它需要的不一致时拒绝启动，所以顺序是 Job 先、Deployment 后；Job 的模板建好后不能改，要先删旧的 Job）。库的版本与二进制要求的必须相等，所以滚动更新的那几秒里，已迁移的库会让旧 Pod 重启时起不来：有迁移的版本，先迁移、再滚动，期间不要重启旧 Pod；目前只有一个迁移。

**副本**：配置了数据库就可以把 `replicas` 调高；两个以上的副本各有自己的读取缓存与滑动窗口，一个调用者在窗口内最多可做 副本数 倍的调用，**每日配额是精确的**。NetworkPolicy 的 `web-database` 放行到 `databases` 命名空间里标签 `app.kubernetes.io/name: postgresql` 的 Pod 的 5432：你的 PostgreSQL 不在那里就改这条的选择器（idp-mcp、idp-host 的清单用同一个），改完 `/readyz` 才会 200。

**数据库不可用时**：有限额的租户的每次搜索与读取被拒绝（`quota_unavailable`，调用没有发出、没有计数）；**配置了数据库的部署里，其余租户的搜索与读取也做不了**（来源编号取不到，`numbering_unavailable`），因为在两个副本之间给不出一致的编号；`/readyz` 503，Pod 退出服务，存活探针不受影响（不会因数据库切换而重启）。数据库回来后无需重启。

**看用量**（所有者或有读权限的角色，库里只有这些）：

```sql
SELECT tenant, kind, used FROM daily_usage WHERE day = (now() AT TIME ZONE 'utc')::date ORDER BY tenant, kind;
```

**清理**：每个副本每 10 分钟删除空闲超过一小时的编号表和超过 35 天的配额日，表不会无限增长。要手工把某租户今天的额度归零：`DELETE FROM daily_usage WHERE tenant = '<租户号>' AND day = (now() AT TIME ZONE 'utc')::date;`（改限额则改租户文件并重启，限额是启动时读的）。
