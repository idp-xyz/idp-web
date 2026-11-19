# ADR-0001　idp-web 的设计（骨架阶段）

| 项 | 内容 |
|---|---|
| 状态 | 已接受（2026-11-19，随任务 W1；平台 ADR-0003 附录 C 的阶段 1 计划） |
| 范围 | `idp-web` 的技术栈、调用者校验、工具声明、正文提取的接口与规则。搜索适配器、`ref`、地址检查、读取、租户配置各由后续任务写入本 ADR |
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
| D5 | **正文提取：接口、纯文本读取器、回退规则、转 Markdown、隐藏内容规则已定；引擎用 readability（3.7，待新页面集确认）** | 见下面 3.1 到 3.6。`internal/extract` 里三个引擎可选（`trafilatura`、`readability`、`plain`），选哪个由评审者在 51 页真实网页和合成 E4 上量出来再定（平台 ADR-0003 附录 C 4.6、第 3 节的判据）；本条不写结论 |

## 3. D5 的细节（任务 W2）

**3.1 接口。** `extract.Extract(html []byte, pageURL string, opts Options) (Result{Title, Markdown, Engine}, error)`；输入已经解码成 UTF-8（解码是读取层的事）。`Options.Engine` 取 `trafilatura`（缺省）、`readability`、`plain`；`Options.Focus` 取 `balanced`（缺省）、`precision`、`recall`，只对 `trafilatura` 有效；`NoFallback` 关掉回退规则，只供评测单看引擎。文本为空是结果，不是错误；错误只给无法使用的选项和无法解析的地址。引擎出错按"什么也没找到"处理。`Page(text, offset, limit) (part, next, total)`：偏移与长度都以字节计，每部分最多 12 KiB，只在字符之间切，`next` 为 -1 表示到头，各部分首尾相接就是全文（与参照实现 `fetch.page` 一致，另保证偏移落在字符中间时前移到下一个字符边界、过小的 `limit` 提到 4 字节以保证前进）。`cmd/idp-web-extract` 是同一个包的命令行，供评测框架通过 `WEB_EVAL_EXTRACTORS` / `WEB_EVAL_EXTRACT_CMD` 打分：标准输入读 HTML，标准输出写 Markdown，标准错误一行 `engine=<谁产出的文本> title=<标题>`，文本为空时状态码也是 0。

**3.2 纯文本读取器（`plain`）。** 取 script、style、noscript、template、svg、nav、footer、header、aside、form 之外的全部文字，标题行加 `#`，p、div、li、tr、br、section、article、table 前换行，空白折叠；规则与参照实现 `tools/web_eval/fetch.py` 的 `_Text` 相同，但建立在已解析的 DOM 上（不是 Python 的事件解析器）。它既是 `plain` 引擎，也是回退规则的比较基准和回退时的结果。

**3.3 回退规则。** 引擎返回空，或者返回的文字（按字符数，不是字节）不足纯文本读取器文字的 10%（`FallbackShare`）时，用纯文本读取器的文字，`Result.Engine` 写 `plain`。规则和数字来自平台 ADR-0003 13.1：在 51 页上，trafilatura 平衡设置加这条回退，段落保留 98.0%、装饰残留 5.9%，回退救下了 trafilatura 什么也提不出或只提出标题的页面（国务院网站的文件页）。注意比较的是两个 Markdown 文本，与参照一致；引擎只返回标题时，只要纯文本读取器的文字也很短，规则不会触发。

**3.4 转 Markdown。** Go 的两个库返回 HTML 节点或纯文本，不返回 Markdown，所以转换是自己写的（`ToMarkdown`，作用在引擎给出的内容节点上）：标题保留层级；段落；嵌套列表（`-` 与 `1.`，下一层缩进）；表格转成管道表（有 `thead` 或第一行有 `th` 时第一行作表头，否则表头为空行，`|` 转义，`colspan` 补空单元格）；`pre`/`code` 转围栏代码块（围栏比代码里最长的反引号串多一个，保留 `language-*`）；引用块；`br` 转硬换行；粗体和斜体保留。链接只留文字、不留地址（平台 ADR-0003 D2：服务不跟随链接）；图片整个丢掉，包括 alt 文字。go-trafilatura 自己建的节点没有 `DataAtom`，转换前按标签名补上。

**3.5 隐藏内容。** 任何引擎之前、在解析后的文档上先删：HTML 注释；带 `hidden` 属性的元素；`aria-hidden="true"` 的元素；内联 `style` 含 `display:none` 或 `visibility:hidden`（不分大小写，允许空白）的元素；属性文字（`alt`、`title`、`aria-label`、`aria-description`、`aria-roledescription`、`placeholder`、`label`）。三个引擎因此看到同一份页面，规则不会因引擎而异；`plain` 也在其中。靠颜色、字号、定位藏起来的文字在不渲染的前提下与可见文字分不开，不处理（这是 E3 里"到达模型"的那几种隐藏方式，以评测结果为准）。每条都有测试：先证明可见文字在，再证明隐藏文字不在，三个引擎、回退开与关都测。

**3.6 新增依赖。** 只有 `internal/extract` 与 `cmd/idp-web-extract` 引用，`cmd/idp-web`（服务本身）还不引用。`go build` 实际编进命令的有 26 个模块（除本模块外）（`go list -m all` 另列出 80 个，多出的是 go-trafilatura 与 go-readability 的测试、示例、命令行依赖，如 go-git、cobra、testify，不进二进制）。直接依赖三个：

| 模块 | 版本 | 许可证 | 拉进来的 |
|---|---|---|---|
| `github.com/markusmobius/go-trafilatura` | v1.12.2（当前最新；模块路径没有 v2） | Apache-2.0 | go-htmldate（Apache-2.0）、go-domdistiller（MIT，它的内置回退之一）、go-shiori/go-readability v1 与 go-shiori/dom（MIT；v1 即已弃用的那个库，是 go-trafilatura 内置回退用的）、go-dateparser（BSD-3-Clause）及其 hablullah/go-hijri、go-juliandays、jalaali/go-jalaali、itlightning/dateparse、araddon/dateparse（MIT）、RadhiFadlillah/whatlanggo、elliotchance/pie、forPelevin/gomoji、gogs/chardet、rivo/uniseg、yosssi/gohtml（MIT）、rs/zerolog 与 mattn/go-colorable、go-isatty（MIT）、andybalholm/cascadia（BSD-2-Clause）、golang.org/x/exp、x/sys、x/text（BSD-3-Clause） |
| `codeberg.org/readeck/go-readability/v2` | v2.1.3（go-shiori/go-readability 的继任者） | MIT | andybalholm/cascadia、go-shiori/dom、gogs/chardet、itlightning/dateparse、golang.org/x/net、x/text |
| `golang.org/x/net` | v0.63.0 | BSD-3-Clause | 解析 HTML（`x/net/html`）；直接依赖，因为转换与隐藏内容规则作用在这个 DOM 上 |

`golang.org/x/net` 升到 v0.63.0 是因为两个库拉进来的 v0.41.0（及 `x/text` v0.26.0）有 10 个已知漏洞：`x/net` 9 个（GO-2026-4440、4441、5025 至 5030、6860、6866，govulncheck 报告它们的调用路径经过 `extract.Extract` 里的 `html.Parse`），`x/text` 1 个（GO-2026-5970）；服务要解析不可信的网页，这些不能留。升级后 `govulncheck ./...` 无漏洞；CI 继续跑它。go-trafilatura 的 `EnableFallback` 缺省开（`NoInnerFallback` 可关）：参照实现的 `trafilatura.extract` 也带内置回退，不开就不是同一个设置；日期搜索关掉（`HtmlDateMode: Disabled`），因为评测与服务都不用日期，且它最慢。wasm 的 RE2（`wasilibs/go-re2`）不在编译范围内，不需要 CGO。

**3.7 引擎的选择（2026-11-19，评审者实测）：用 `readability`（`codeberg.org/readeck/go-readability/v2`），待新页面集确认。**

判据在测之前写定（平台 ADR-0003 附录 C 4.6）：51 页真实网页上正文段保留 ≥ 95%、页面装饰残留 ≤ 10%，合成 E4 全过；达标的候选里选依赖最少、部署最简单的。

| 设置 | 正文段保留 | 每段都保留的页 | 装饰残留 | 完全没有装饰的页 | 回退到纯文本读取器 | 合成 E4 |
|---|---|---|---|---|---|---|
| Python trafilatura 平衡（参照） | 98.0% | 94.1% | 5.9% | 90.2% | 2 页 | 35/35 |
| go-trafilatura 平衡 | 98.0% | 94.1% | 2.0% | 94.1% | 0 | 35/35 |
| go-trafilatura 平衡，关掉内置回退 | 97.0% | 92.2% | 2.0% | 94.1% | 0 | 未跑 |
| go-readability | 98.0% | 94.1% | 3.9% | 96.1% | 1 页 | 35/35 |
| 纯文本读取器 | 100% | 100% | 52.9% | 21.6% | — | 未跑 |

- **两个 Go 候选都达标，而且都不差于 Python 参照。** 两者之间的差别是 153 处装饰里的 3 处和 6 处，51 页分不出高下。
- **按写定的规则选依赖少的。**
  - readability 编进二进制的第三方模块是 6 个。
  - go-trafilatura 约 20 个，其中有已弃用的 `go-shiori/go-readability` v1；它的好成绩有一部分来自内置回退到这个 v1（关掉后保留率从 98.0% 降到 97.0%）。
- **两者各自的失手**（都不做针对页面的修补）：
  - readability：GitHub issue 页丢了靠后的评论；两个正文极短的页面（顺丰通知、一段话的新闻）留下了全部 3 处装饰。
  - go-trafilatura：Kubernetes 文档丢了引言；三个政府网站页面各留 1 处页脚或位置提示。
- **E3 的 180 个注入案例**：两个引擎下，各隐藏方式能否到达模型与参照完全一致（6 种到达、4 种被去掉），答案在 180 个案例里都还在。
- **表格**：四个带表格的真实页面上，两个引擎都保留为 Markdown 表格。
- **还没做的**：这 51 页已经用来选过一次回退规则，现在又用来选引擎。平台计划的 V2 任务另采约 30 页，只做确认；readability 在新页面上达不到判据时，回到这里重选。在那之前，两个引擎的代码都留着；确认之后删掉落选的引擎和它的依赖。

## 4. 其余

搜索、`ref`、地址检查、读取、提取、租户配置：见平台 ADR-0003 与它的附录 C。
