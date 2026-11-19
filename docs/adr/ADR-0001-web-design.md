# ADR-0001　idp-web 的设计（骨架阶段）

| 项 | 内容 |
|---|---|
| 状态 | 已接受（2026-11-19，随任务 W1；平台 ADR-0003 附录 C 的阶段 1 计划） |
| 范围 | `idp-web` 的技术栈、调用者校验、工具声明。搜索适配器、`ref`、地址检查、读取、正文提取、租户配置各由后续任务写入本 ADR |
| 依据 | 平台 ADR-0003（网页搜索与读取）D1、D2；idp-sandbox ADR-0001 D10、D18；idp-mcp ADR-0001 D40、D44；MCP 2026-07-28 |

## 1. 背景与边界

平台 ADR-0003 决定新增 idp-web：Agent 搜索网页、读取网页的上游服务，放在 idp-mcp 网关后面，与 idp-sandbox 并列。本 ADR 只记仓库自己的决策。为什么要有这个组件、外泄通道怎么切断、`ref` 和地址检查的设计，都在平台 ADR-0003，这里不重复。

本任务（W1）只建骨架：服务能启动、能校验调用者、能列出三个工具；工具本身不工作。

## 2. 决策

| # | 决策 | 理由 |
|---|---|---|
| D1 | **照 idp-sandbox 的方式搭：Go，MCP 手写，骨架阶段只用标准库；MCP 层从沙箱复制，不共享代码** | 沙箱 D10 已做过同样的选择：协议准入规则是 idp-mcp 网关的规则，两边各复制一份，比引入一个共享模块（要版本、要发布、要三个仓库同时升级）简单。复制会漂移，所以把漂移变成测试失败：`internal/mcp` 带着沙箱的同一组断言向量，由 idp-mcp 的签名器在固定时刻为固定请求签出，同一个请求和摘要也钉在 idp-mcp 的 `TestRequestDigestIsTheOneUpstreamsCompute` 里；哪一边改了规范化或校验规则，这条测试就会失败。向量里的受众（`http://sandbox:8570/mcp`）和工具名（`sandbox_run`）是被签名的内容，所以原样保留，不改成 web。后续任务的第三方依赖（HTML 解析、正文提取）在各自的决策里写明理由，CI 跑 govulncheck |
| D2 | **调用者只取自网关签名的断言** | 设了 `IDP_WEB_CALLER_KEYS_FILE` 与 `IDP_WEB_CALLER_AUDIENCE` 之后，每个 `tools/call` 必须带 `_meta["io.idp.telos/caller-assertion"]`：ES256、`typ` 为 `idp-caller+jwt`、`kid` 是持有的公钥、签名验得过、`aud` 是本服务、没过期（允许 30 秒时钟差，有效期超过 5 分钟的不收）、不是未来签的、`req` 等于这次请求的摘要（参数与签名时不同即拒绝）、`jti` 只收一次；公钥必须是 P-256。没有断言得到 HTTP 403，工具不会运行；`tools/list` 与 `server/discover` 不需要调用者（网关的漂移检查没有）。租户和成员只来自校验过的调用者，不是工具参数：`ref` 要绑定调用者（ADR-0003 4.3），没有这一条它就没有意义。公钥与受众只设一个，启动即退出（状态码 2）。`IDP_WEB_REQUIRE_CALLER` 缺省为真。每一种拒绝在 `internal/mcp` 里是一个有名字的反例，并有一个正例在先，证明拒绝出于预期的原因 |
| D3 | **三个工具的参数与标注先定下来，处理程序以后再换** | 网关在评审时钉住每个工具的 `inputSchema` 与标注，之后的任何变化都要重新评审（idp-mcp 的漂移封锁）。先把最终声明定下并放进测试，后面的任务只换处理程序，不会因改参数而反复评审。`web_search {query, freshness?, count?}`（`count` 为 1 到 10，`freshness` 取 `day`、`week`、`month`、`year`）；`web_fetch {url, ref, offset?}`；`web_open_url {url, offset?}`；都是 `additionalProperties: false`，标注都是 `readOnlyHint: true`、`openWorldHint: true`。参数严格解码，未知字段是错误。当前每个处理程序在校验参数之后返回固定的错误句子"web search is not available yet"（`isError: true`）；错误句子是哨兵错误到消息的映射，其他错误只进日志，模型得到的是固定的一句 |
| D4 | **审计只记元数据，调用者记短哈希** | 每次调用一行日志：工具、结果、耗时、调用者（租户与成员的 SHA-256 前 4 字节）。不记成员标识本身，不记参数内容。查询词和地址的审计字段由后续任务按 ADR-0003 4.7 加入 |

## 3. 其余

搜索、`ref`、地址检查、读取、提取、租户配置：见平台 ADR-0003 与它的附录 C。
