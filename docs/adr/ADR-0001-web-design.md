# ADR-0001　idp-web 的设计（骨架阶段）

| 项 | 内容 |
|---|---|
| 状态 | 已接受（2026-11-19，随任务 W1；平台 ADR-0003 附录 C 的阶段 1 计划） |
| 范围 | `idp-web` 的技术栈、调用者校验、工具声明、正文提取的接口与规则、地址规则与读取（D6 到 D15，任务 W3）。搜索适配器、`ref`、租户配置各由后续任务写入本 ADR |
| 依据 | 平台 ADR-0003（网页搜索与读取）D1、D2；idp-sandbox ADR-0001 D10、D18；idp-mcp ADR-0001 D40、D44；平台 ADR-0003 D4、7.3（T3、T7）、13.1；idp-sandbox ADR-0001 D22；RFC 9309；MCP 2026-07-28 |

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
| D5 | **正文提取：接口、纯文本读取器、回退规则、转 Markdown、隐藏内容规则已定；引擎用 readability（3.7，已由新页面集确认）** | 见下面 3.1 到 3.7。`internal/extract` 里有两个引擎：`readability`（缺省）和 `plain`。选择在 51 页真实网页和合成 E4 上量出来（平台 ADR-0003 附录 C 4.6、第 3 节的判据），再由另采的 30 页确认；落选的 go-trafilatura 和它的依赖已从代码里删除（它的实现在提交 7e094bb 里） |
| D6 | **哪些地址算公网：IANA 特殊用途登记表里"不可全球路由"的一律拒绝，不在表里的保留空间和带区的地址也拒绝** | 表写成 `internal/addr` 里的一张数据表，注明登记表与日期（2026-11-19），从 idp-sandbox 出口代理的表改写而来（两个仓库不共享代码，改一处要手工改另一处）：IPv4 的 14 块、IPv6 的 11 块，含 NAT64、6to4、Teredo 整块拒绝（它们能到达里面嵌的 IPv4 地址）；IPv4 映射的 IPv6 地址按它携带的 IPv4 地址判断；任何带区的地址一律拒绝（`netip.Prefix.Contains` 对带区地址恒为假，只查表会放行）。比沙箱多一条：2000::/3 之外的 IPv6 空间（登记表没有逐条列出的未分配空间）也拒绝，因为没有网站会在那里。相对 Python 参照 `is_global` 因此更严，差异只在这一段。测试：`TestEveryBlockOfTheRegistryTableIsNotPublicAtItsFirstMiddleAndLastAddress`（用表本身生成每块的首、中、尾地址）、`TestAddressesThatAreNotPublicAreNotPublic`、`TestEveryZonedAddressIsNotPublic`、`TestPublicAddressesNextToTheSpecialPurposeBlocksStayPublic`（相邻的公网地址仍放行，防止过严）、`TestEmbeddedIPv4ShowsWhatATranslatedAddressWouldReach` |
| D7 | **一个解析器，一次解析：`addr.Target` 返回之后的每一步只用它给的协议、主机、端口** | 两个解析器对同一段文字的理解不同，是 SSRF 的常见来源。`Target` 用 `net/url` 解析一次，拒绝：http 和 https 以外的协议、无 `//` 的地址、带用户名密码的地址（包括空的）、80 和 443 以外的端口、空主机、IDNA 转换失败的主机；国际化域名先转成 ASCII 形式；十进制、十六进制、八进制、短写的 IP（`2130706433`、`0x7f.1`、`0177.0.0.1`、`127.1`，判据是最后一个标签是数字，与 WHATWG 的 IPv4 判断相同）一律拒绝，因为 Go 自己的解析器把它们当域名，C 库和浏览器当 127.0.0.1；全角数字和句号经 IDNA 映射成的 IP 同样拒绝。**规范写法的 IP 字面量：公网的放行，非公网的在这里就拒绝**（不等连接）；IPv6 字面量必须是规范写法且不带区。请求的地址由 `Address.URL()` 从这几个部分重建，不再用原文，所以检查过的与请求的是同一个。测试：`TestAddressesAreRefusedForTheReasonTheyAreWrong`、`TestTheTwentyAddressesOfTheSSRFListAreRefused`（评测的 20 个）、`TestAddressesThatMayBeReadAreParsedOnceIntoTheirParts`、`TestTheParsedAddressRoundTripsThroughItsOwnURL`、模糊测试 `FuzzTarget`（不崩溃；接受的地址协议为 http/https、端口 80/443、无用户信息、主机是名字或公网规范 IP；60 秒结果见任务报告） |
| D8 | **钉住地址的连接：Go 自己的解析器，每个连接解析一次，任何一个答案不是公网就拒绝，只连检查过的那个 IP，不用代理** | `fetch.checkedDial` 是读取器唯一的建连方式：解析（`net.Resolver{PreferGo: true}`，可注入）、检查**全部**答案（参照实现同样；一个名字有一个私网答案，就是攻击者能控制的名字）、再用 IP 去连，不再解析名字，所以"检查时公网、连接时私网"的 DNS 重绑定到不了内网；TLS 仍由传输层对请求里的主机名验证证书。HTTP 客户端 `Proxy: nil`，不读代理环境变量：代理会自己解析、自己连接，对地址的检查就没有意义（平台计划第 2 节第 8 条；必须经出口代理的部署不在阶段 1）。每个请求一个连接（关闭 keep-alive），所以每个请求都重新解析、重新检查。拨号处把端口和字面量再检查一遍，不依赖解析器已拒绝过。返回给模型的错误不写解析出的地址（内网地址不能经错误泄露），只写日志。测试：`TestANameWithAPrivateAnswerAmongItsPublicOnesIsRefusedWithoutAConnection`、`TestRebindingTheConnectionGoesToTheCheckedAddressAndTheNameIsLookedUpOnce`（第一次答公网、再问就答 127.0.0.1；只查一次、只连检查过的地址）、`TestEveryConnectionIsResolvedOnceAndConnectedToWhatWasChecked`、`TestTheCheckedDialRefusesWhatTheParserWouldHaveRefusedItself`、`TestTheTwentyAddressesOfTheSSRFListAreRefusedWithoutAConnection`、`TestProxyEnvironmentVariablesAreIgnored` |
| D9 | **重定向与限额：每一跳重走同一套检查，最多 5 次；5 MiB（解码后同样）、15 秒、响应头 64 KiB** | 重定向由 `follow` 自己跟（客户端的跟随关掉），每一跳都过 `Target`、`Allow` 钩子、robots.txt 和钉住地址的连接；第 6 次重定向被拒绝，报告最终地址。15 秒是一次 `Read` 的总时间，连接、响应头、正文以及这次读取要取的 robots.txt 都在里面（比参照的"每个请求 15 秒"更严）。正文先看 `Content-Length`，再按 5 MiB 加一字节读，超过即拒绝。重定向到 `unblock.` 开头的主机按验证页处理（联邦公报）。`Allow func(host string) error` 对第一个地址和每次重定向调用，返回的错误原样给调用者（任务 W4 用它执行租户域名名单）。测试：`TestFiveRedirectsAreFollowedAndTheFinalAddressIsReported`、`TestAChainOfSixRedirectsIsRefused`、`TestARedirectToAPrivateAddressIsRefusedAndNeverFollowed`、`TestARelativeRedirectIsResolvedAgainstTheAddressItCameFrom`、`TestABodyOverFiveMiBIsRefusedWhetherOrNotItsLengthIsAnnounced`、`TestABodyThatArrivesTooSlowlyIsRefusedAtTheDeadline`、`TestAServerThatNeverAnswersIsRefusedAtTheDeadline`、`TestResponseHeadersAreBounded`、`TestTheAllowHookSeesTheFirstAddressAndEveryRedirectAndCanRefuseThem` |
| D10 | **类型、压缩与字符集：只读 HTML、XHTML、纯文本；gzip 与 deflate 自己解，解码后同样限 5 MiB；字符集解成 UTF-8** | PDF 单独报错（"这是 PDF，暂不支持"），其他类型报"content type … is not read"，没有类型也拒绝（参照同）。自己发 `Accept-Encoding: gzip, deflate` 并关掉传输层的自动解压，上限才由我们定：解码器通过限长读取，永远不读到尽头，几 KB 的压缩炸弹展开不到 5 MiB 以上。deflate 先按 zlib 包装解，失败再按裸流解（参照同，真实网站有这样发的）。其他编码（br、zstd、叠加的 `gzip, br`）和解不开的数据拒绝。字符集按 Content-Type、字节序标记、文档 meta 的次序取（`x/net/html/charset`）；都没声明而字节是合法 UTF-8 时按 UTF-8（库对未声明页面的缺省是 windows-1252，会把不声明字符集的现代页面读成乱码）。新增依赖：`golang.org/x/text` 由间接变直接（BSD-3-Clause，字符集与测试里的 GBK 编码）。测试：`TestContentTypesThatAreNotTextAreRefusedWithTheirOwnMessages`、`TestOnlyGzipAndDeflateAreDecodedAndAnythingElseIsRefused`（含裸 deflate、损坏数据）、`TestACompressionBombIsRefusedByItsDecodedSize`、`TestPagesAreDecodedToUTF8ByTheirDeclaredCharset`（GBK 的头、meta 两种、无声明的 UTF-8、字节序标记） |
| D11 | **robots.txt 按 RFC 9309，不按参照实现；自己写，不加依赖** | 用户代理分组（我们的产品名 `idp-web` 与 `*`；有分组点名我们时只用点名的，各组合并），Allow/Disallow 取最长匹配、平手 Allow 胜、`*` 与 `$`、百分号编码按标准形式比较。经同一条钉住地址的路径取（起点与页面在同一来源，已过页面的检查；robots.txt 的重定向再过 `Allow`），最多读 512 KiB（超出的部分不读，不是整个拒绝，RFC 2.5 要求至少读前 500 KiB），按来源（协议、主机、端口）缓存一小时，最多 1024 个来源，满了先清过期的、再清最先到期的。**与 Python 参照的差别：** 参照对任何读不到的 robots.txt 都按"全部允许"；RFC 2.3.1.3 说 4xx 才是"没有限制"，2.3.1.4 说 5xx 或不可达要按"整站不可抓"处理。一个只在好读的时候才遵守 robots.txt 的读取器，站点没法信任它，所以按 RFC：4xx 放行，5xx、连不上、读不出都拒绝（错误说明是 robots.txt 读不到）；重定向超过 5 次按不可用处理，放行。读不到的结果只缓存一分钟，免得每个页面都去问一个坏掉的站点。若 robots.txt 的地址本身是不可连的（非公网），页面的真实错误是"地址不是公网"，原样返回。测试：`TestRobotsRulesFollowRFC9309`（34 条表驱动）、`TestRobotsTxtDecidesWhetherAPageIsRead`、`TestRobotsTxtOfFourHundredsMeansNoRestrictionsAndOfFiveHundredsOrNoAnswerMeansNoReading`（含 5xx、连接被切断、损坏的压缩）、`TestRobotsTxtIsFetchedThroughTheSameCheckedPath`、`TestRobotsTxtIsCachedPerOriginForAnHourAndFailuresForAMinute`、`TestTheRobotsCacheIsBounded`、`TestRobotsTxtIsReadUpToItsLimitAndNotRefusedForBeingLarger` |
| D12 | **验证页（反爬页）不是页面正文：已知标记且可见文字很少时报"该网站返回了验证页"** | 挑战页常以 200 返回，交给模型它会被当成页面来总结，比报错更糟。标记清单只在 `internal/fetch/botcheck.go` 一处，每条写明在哪里见过：Cloudflare（`_cf_chl_opt`、`cf-chl-`、`cf-browser-verification`、标题"Just a moment..."、"Attention Required! \| Cloudflare"，响应头 `Cf-Mitigated: challenge`）、AWS WAF（`awsWafCookieDomainList`、`gokuProps`；eur-lex 以 200 返回 2035 字节的挑战页）、联邦公报（重定向到 `unblock.` 主机）、字节与火山引擎（标题"Please wait..."、"正在进行安全检测"）、以及可见文字在 300 字以内、内容就是验证码提示的短页（"verify you are human"、"captcha"、"人机验证"等）。**误报的防线是可见文字的长度**：有标记的页面，可见文字超过 1000 字符就是页面（真实文章会提到验证码，许多 Cloudflare 站点每页都带检测脚本）；提示类文字再收紧到 300 字符。可见文字取提取之后的正文，即模型本会拿到的文字。403、429、503 的答案也看正文（挑战页常这样返回），否则"HTTP 403"掩盖了真实原因。测试：`TestEachBotCheckMarkerMakesAShortPageABotCheck`（每个标记一页）、`TestLongRealArticlesThatMentionBotChecksAreNotBotChecks`（长文章提到验证码、带检测脚本、标题恰为"Just a moment..."）、`TestTheBoundOnVisibleTextIsWhatKeepsAMarkerFromBeingAVerdict`、`TestAChallengeAnsweredWithAnErrorStatusOrAHeaderIsABotCheckNotAStatus`、`TestARedirectToAnUnblockHostIsABotCheck` |
| D13 | **TLS 根证书：用 `golang.org/x/crypto/x509roots/fallback`，在 `cmd/idp-web` 里为副作用导入** | 镜像是 `FROM scratch`，没有 CA 证书，HTTPS 会失败。两条路里选这一条：不用往镜像里复制文件（`deploy/build.sh` 不依赖主机的证书目录，镜像仍只有一个文件），根证书随 Go 模块版本固定，进入 `go.sum` 和 govulncheck 的视野，构建仍可离线重现；系统有根证书时 Go 优先用系统的，所以在有证书的环境里行为不变。更新方式：`go get golang.org/x/crypto/x509roots/fallback@latest`，重建镜像；包是从 Mozilla NSS 根存储生成的，当前版本 v0.0.0-20261115193255-739070949446（2026-11-15），许可证 BSD-3-Clause（Go 作者；证书数据来自 NSS）。跟不上 NSS 的撤销是这条路的代价，所以要随依赖更新（CI 已跑 govulncheck，另应按月更新）。证明：不是单元测试，而是在镜像里跑 `idp-web -check-tls https://example.org/`（任务报告里有输出，过期证书的站点被拒绝）；`TestTLSVerifiesTheHostNameAndTheChain` 另证主机名与证书链是被验证的 |
| D14 | **测试用例外：`IDP_WEB_TEST_ORIGINS` 只豁免列出的 `主机:端口`，其余一概不豁免** | 逗号分隔的精确 `主机:端口`，在 `cmd/idp-web` 解析（通配符、缺端口、空主机、带区的地址都是错误：松散的条目豁免得比本意多），传给读取器。只豁免端口规则与公网规则；用户名密码、协议、非规范写法仍然拒绝；同一主机的另一个端口、同一端口的另一个主机、重定向到别处都不豁免。服务启动时若设了它，日志警告。**还没做：** 计划说"清单测试断言发布的清单里没有它"；本仓库没有部署清单，这条断言属于 compose 与 kind 部署的任务。测试：`TestATestOriginIsExemptAndNothingElseIs`、`TestTestOriginsExemptExactlyTheListedHostAndPortAndNothingElse`、`TestTestOriginsAreParsedIntoTheFormTheReaderComparesWith`、`TestATestOriginThatCouldExemptMoreThanWasMeantIsAnError` |
| D15 | **错误是模型能据以行动的小写句子，内部细节只进日志** | `ErrNotPublic`（`addr.ErrNotPublic`，字面量与解析结果同一个）、`ErrRobots`、`ErrTooLarge`、`ErrTooSlow`、`ErrType`、`ErrPDF`、`ErrEncoding`、`ErrBotCheck`、`ErrStatus`（附状态码）、`ErrTooManyRedirects`、`ErrConnect`、`ErrUnreadable`，加 `addr.ErrRefused`（地址形式，附原因）；用 `errors.Is` 判断。连接错误、解析出的地址、传输层的原文不进错误，只进日志。调用者取消时返回 `context.Canceled`，不当作超时。测试：各条错误对应的测试同上；`TestTheCallersCancellationIsReturnedAsItIs`、`TestAConnectionThatCannotBeMadeIsReportedWithoutItsInternals` |

## 3. D5 的细节（任务 W2）

**3.1 接口。** `extract.Extract(html []byte, pageURL string, opts Options) (Result{Title, Markdown, Engine}, error)`；输入已经解码成 UTF-8（解码是读取层的事）。`Options.Engine` 取 `readability`（缺省）、`plain`；`NoFallback` 关掉回退规则，只供评测单看引擎。文本为空是结果，不是错误；错误只给无法使用的选项和无法解析的地址。引擎出错按"什么也没找到"处理。`Page(text, offset, limit) (part, next, total)`：偏移与长度都以字节计，每部分最多 12 KiB，只在字符之间切，`next` 为 -1 表示到头，各部分首尾相接就是全文（与参照实现 `fetch.page` 一致，另保证偏移落在字符中间时前移到下一个字符边界、过小的 `limit` 提到 4 字节以保证前进）。`cmd/idp-web-extract` 是同一个包的命令行，供评测框架通过 `WEB_EVAL_EXTRACTORS` / `WEB_EVAL_EXTRACT_CMD` 打分：标准输入读 HTML，标准输出写 Markdown，标准错误一行 `engine=<谁产出的文本> title=<标题>`，文本为空时状态码也是 0。

**3.2 纯文本读取器（`plain`）。** 取 script、style、noscript、template、svg、nav、footer、header、aside、form 之外的全部文字，标题行加 `#`，p、div、li、tr、br、section、article、table 前换行，空白折叠；规则与参照实现 `tools/web_eval/fetch.py` 的 `_Text` 相同，但建立在已解析的 DOM 上（不是 Python 的事件解析器）。它既是 `plain` 引擎，也是回退规则的比较基准和回退时的结果。

**3.3 回退规则。** 引擎返回空，或者返回的文字（按字符数，不是字节）不足纯文本读取器文字的 10%（`FallbackShare`）时，用纯文本读取器的文字，`Result.Engine` 写 `plain`。规则和数字来自平台 ADR-0003 13.1：在 51 页上，trafilatura 平衡设置加这条回退，段落保留 98.0%、装饰残留 5.9%，回退救下了 trafilatura 什么也提不出或只提出标题的页面（国务院网站的文件页）。注意比较的是两个 Markdown 文本，与参照一致；引擎只返回标题时，只要纯文本读取器的文字也很短，规则不会触发。

**3.4 转 Markdown。** Go 的两个库返回 HTML 节点或纯文本，不返回 Markdown，所以转换是自己写的（`ToMarkdown`，作用在引擎给出的内容节点上）：标题保留层级；段落；嵌套列表（`-` 与 `1.`，下一层缩进）；表格转成管道表（有 `thead` 或第一行有 `th` 时第一行作表头，否则表头为空行，`|` 转义，`colspan` 补空单元格）；`pre`/`code` 转围栏代码块（围栏比代码里最长的反引号串多一个，保留 `language-*`）；引用块；`br` 转硬换行；粗体和斜体保留。链接只留文字、不留地址（平台 ADR-0003 D2：服务不跟随链接）；图片整个丢掉，包括 alt 文字。go-trafilatura 自己建的节点没有 `DataAtom`，转换前按标签名补上。

**3.5 隐藏内容。** 任何引擎之前、在解析后的文档上先删：HTML 注释；带 `hidden` 属性的元素；`aria-hidden="true"` 的元素；内联 `style` 含 `display:none` 或 `visibility:hidden`（不分大小写，允许空白）的元素；属性文字（`alt`、`title`、`aria-label`、`aria-description`、`aria-roledescription`、`placeholder`、`label`）。三个引擎因此看到同一份页面，规则不会因引擎而异；`plain` 也在其中。靠颜色、字号、定位藏起来的文字在不渲染的前提下与可见文字分不开，不处理（这是 E3 里"到达模型"的那几种隐藏方式，以评测结果为准）。每条都有测试：先证明可见文字在，再证明隐藏文字不在，三个引擎、回退开与关都测。

**3.6 依赖。** `internal/extract` 与 `cmd/idp-web-extract` 引用下面两个第三方库（`cmd/idp-web` 从任务 W3 起另引用 x/crypto 的根证书与 x/text，见 D10、D13）。go-trafilatura 及其依赖在选定 readability 之后已删除（任务 W3，见 3.7）：编进 `idp-web-extract` 的第三方模块从 26 个降到 7 个（go-readability/v2、andybalholm/cascadia、go-shiori/dom、gogs/chardet、itlightning/dateparse、golang.org/x/net、x/text）；`idp-web` 是这 7 个加 x/crypto/x509roots/fallback，共 8 个。`go list -m all` 里 go-trafilatura 的测试、示例、命令行依赖（go-git、cobra、testify 等）也一并离开 `go.mod`。

| 模块 | 版本 | 许可证 | 拉进来的 |
|---|---|---|---|
| `codeberg.org/readeck/go-readability/v2` | v2.1.3（go-shiori/go-readability 的继任者） | MIT | andybalholm/cascadia、go-shiori/dom、gogs/chardet、itlightning/dateparse、golang.org/x/net、x/text |
| `golang.org/x/net` | v0.63.0 | BSD-3-Clause | 解析 HTML（`x/net/html`）与 IDNA、字符集；直接依赖，因为转换与隐藏内容规则作用在这个 DOM 上 |

`golang.org/x/net` 升到 v0.63.0 是因为两个库拉进来的 v0.41.0（及 `x/text` v0.26.0）有 10 个已知漏洞：`x/net` 9 个（GO-2026-4440、4441、5025 至 5030、6860、6866，govulncheck 报告它们的调用路径经过 `extract.Extract` 里的 `html.Parse`），`x/text` 1 个（GO-2026-5970）；服务要解析不可信的网页，这些不能留。升级后 `govulncheck ./...` 无漏洞；CI 继续跑它。

转 Markdown 里两处原为 go-trafilatura 的节点而写的规则保留：`normalizeAtoms`（补 `DataAtom`；readability 的输出里有一个这样的节点，它的包装 `div`，所以仍作保护）和 `isCodeBlock`（作者也会写出裸的多行 `code`，且 3.7 量过的配置里就有这条规则）。去掉它们，现有测试仍过；留着是为了不改变量过的行为。

**3.7 引擎的选择（2026-11-19，评审者实测）：用 `readability`（`codeberg.org/readeck/go-readability/v2`），已由新页面集确认。**

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
- **新页面集的确认（2026-11-19）**：另采 30 页（中英各 15，站点与前 51 页不重复；只用于确认，不参与任何选择）。readability 正文段保留 97.4%、装饰残留 6.7%，达到判据（≥ 95%、≤ 10%）；Python 参照为 96.6% 和 6.7%。选择确认。
- **如实记录的差别**：go-trafilatura 在这 30 页上是 98.3% 和 1.1%，在前 51 页上是 98.0% 和 2.0%。两批页面上它的装饰残留都低于 readability（3.9%、6.7%），方向一致；按页数看两者相近（有残留的页面：readability 共 5 页，go-trafilatura 共 4 页），readability 失手时往往把一页的 3 处全留下。没有因此改选：判据和"达标后选依赖少的"是在测量之前写定的，事后按结果换规则会让判据失去意义；而依赖的差别（6 个模块对约 20 个，后者含一个已弃用的库）是确定的，装饰残留的差别在 81 页上还分不清是不是偶然。
- **以后重选的条件**：引擎在 `internal/extract` 的接口后面，平台的评测（`run e4-real`，`WEB_EVAL_EXTRACTORS`）可以随时重测。真实使用中页面装饰进入回答成为问题时，用更大的页面集重测 go-trafilatura（提交 7e094bb 里有它的实现）。
- 落选的 go-trafilatura 引擎和它的依赖已从代码里删除。

## 4. 反向验证（任务 W3）

D6 到 D15 的每条控制，各去掉一次、看测试失败、再恢复（脚本逐条改源码、跑对应包的测试、恢复；改动都是一两行，如把条件换成 `false`）。下表给出去掉了什么和失败的测试（多于三个的只列前三个）。表里有三处**没有**单独失败，原因在表后。

| 控制 | 去掉了什么 | 失败的测试 |
|---|---|---|
| D6 表项 | 169.254.0.0/16（云元数据） | `TestEveryBlockOfTheRegistryTableIsNotPublicAtItsFirstMiddleAndLastAddress`、`TestAddressesThatAreNotPublicAreNotPublic`、`TestTheTwentyAddressesOfTheSSRFListAreRefused` |
| D6 表项 | 2002::/16（6to4） | `TestEveryBlockOfTheRegistryTableIsNotPublicAtItsFirstMiddleAndLastAddress`、`TestEmbeddedIPv4ShowsWhatATranslatedAddressWouldReach` |
| D6 表项 | ::/8 并同时去掉 2000::/3 规则 | `TestAddressesThatAreNotPublicAreNotPublic`、`TestEmbeddedIPv4ShowsWhatATranslatedAddressWouldReach`、`TestTheTwentyAddressesOfTheSSRFListAreRefused` |
| D6 表项 | fc00::/7 并同时去掉 2000::/3 规则 | `TestAddressesThatAreNotPublicAreNotPublic`、`TestTheTwentyAddressesOfTheSSRFListAreRefused` |
| D6 带区地址 | 区检查并同时去掉 2000::/3 规则 | `TestEveryZonedAddressIsNotPublic` |
| D6 2000::/3 | 2000::/3 规则 | `TestAddressesThatAreNotPublicAreNotPublic`（`4000::1`） |
| D6 IPv4 映射 | 先取出 IPv4 地址 | `TestPublicAddressesNextToTheSpecialPurposeBlocksStayPublic`（`::ffff:8.8.8.8`） |
| D7 用户名密码 | 拒绝用户信息 | `TestAddressesAreRefusedForTheReasonTheyAreWrong`、`TestTheTwentyAddressesOfTheSSRFListAreRefused` |
| D7 端口 | 拒绝 80、443 以外的端口 | `FuzzTarget`、`TestAddressesAreRefusedForTheReasonTheyAreWrong`、`TestTheTwentyAddressesOfTheSSRFListAreRefused` 等 |
| D7 协议 | 拒绝 http、https 以外的协议 | 同上 |
| D7 非规范 IP | `looksNumeric` 的判断 | `FuzzTarget`、`TestAddressesAreRefusedForTheReasonTheyAreWrong`、`TestTheTwentyAddressesOfTheSSRFListAreRefused` 等 |
| D7 IDNA | 国际化域名转换 | `TestAddressesAreRefusedForTheReasonTheyAreWrong`、`TestAddressesThatMayBeReadAreParsedOnceIntoTheirParts` |
| D7 字面量 | 对 IP 字面量检查公网 | `FuzzTarget`、`TestAddressesAreRefusedForTheReasonTheyAreWrong`、`TestTheTwentyAddressesOfTheSSRFListAreRefused` 等 |
| D7 IPv6 规范写法 | 要求规范写法 | `TestAddressesAreRefusedForTheReasonTheyAreWrong` |
| D8 全部答案 | 只检查第一个答案 | `TestANameWithAPrivateAnswerAmongItsPublicOnesIsRefusedWithoutAConnection` |
| D8 公网检查 | 拨号处整个检查 | `TestANameWithAPrivateAnswerAmongItsPublicOnesIsRefusedWithoutAConnection`、`TestARedirectToAPrivateAddressIsRefusedAndNeverFollowed`、`TestTheCheckedDialRefusesWhatTheParserWouldHaveRefusedItself` 等 |
| D8 钉住地址 | 连接时用名字（再解析一次）而不是检查过的 IP | `TestRebindingTheConnectionGoesToTheCheckedAddressAndTheNameIsLookedUpOnce`、`TestEveryConnectionIsResolvedOnceAndConnectedToWhatWasChecked`、`TestANameWithAPrivateAnswerAmongItsPublicOnesIsRefusedWithoutAConnection` |
| D8 不用代理 | `Proxy: nil` 换成 `ProxyFromEnvironment` | `TestProxyEnvironmentVariablesAreIgnored`（另有 6 个因连接走了代理而失败） |
| D8 每连接一次 | 打开 keep-alive | `TestEveryConnectionIsResolvedOnceAndConnectedToWhatWasChecked` |
| D8 字面量与重定向 | 解析器对字面量的检查和拨号处的检查同时去掉 | `TestARedirectToAPrivateAddressIsRefusedAndNeverFollowed`、`TestTheTwentyAddressesOfTheSSRFListAreRefusedWithoutAConnection`、`TestTheCheckedDialRefusesWhatTheParserWouldHaveRefusedItself` 等 |
| D9 重定向次数 | 上限 5 改为 6；去掉上限 | `TestAChainOfSixRedirectsIsRefused` |
| D9 `Allow` 钩子 | 第一个地址不调用；重定向不调用 | `TestTheAllowHookSeesTheFirstAddressAndEveryRedirectAndCanRefuseThem`（各一次） |
| D9 `unblock.` | 跟随重定向 | `TestARedirectToAnUnblockHostIsABotCheck` |
| D9 正文上限 | 不截断也不看 `Content-Length` | `TestABodyOverFiveMiBIsRefusedWhetherOrNotItsLengthIsAnnounced` |
| D9 总时限 | 去掉 15 秒的超时 | `TestABodyThatArrivesTooSlowlyIsRefusedAtTheDeadline` |
| D9 响应头 | 去掉 `MaxResponseHeaderBytes` | `TestResponseHeadersAreBounded` |
| D10 解码上限 | 去掉解码后的限长 | `TestACompressionBombIsRefusedByItsDecodedSize` |
| D10 类型 | 任何类型都读；PDF 不单独报错 | `TestContentTypesThatAreNotTextAreRefusedWithTheirOwnMessages`（各一次） |
| D10 编码 | 未知编码原样放过 | `TestOnlyGzipAndDeflateAreDecodedAndAnythingElseIsRefused` |
| D10 裸 deflate | 不再尝试裸流 | `TestOnlyGzipAndDeflateAreDecodedAndAnythingElseIsRefused` |
| D10 自己发 `Accept-Encoding` | 不发 | `TestAPageIsReadAsMarkdownWithItsTitleAndTypeAndTheReaderIdentifiesItself`、`TestOnlyGzipAndDeflateAreDecodedAndAnythingElseIsRefused` |
| D10 字符集 | 一律按 UTF-8 | `TestPagesAreDecodedToUTF8ByTheirDeclaredCharset` |
| D11 robots.txt | 不查 | `TestRobotsTxtDecidesWhetherAPageIsRead` 等 8 个 |
| D11 5xx | 按"没有限制" | `TestRobotsTxtOfFourHundredsMeansNoRestrictionsAndOfFiveHundredsOrNoAnswerMeansNoReading`、`TestRobotsTxtIsCachedPerOriginForAnHourAndFailuresForAMinute` |
| D11 不可达 | 按"没有限制"（参照实现的做法） | `TestRobotsTxtOfFourHundredsMeansNoRestrictionsAndOfFiveHundredsOrNoAnswerMeansNoReading`、`TestAConnectionThatCannotBeMadeIsReportedWithoutItsInternals` |
| D11 4xx | 按"整站不可抓" | 几乎所有读页面的测试（26 个） |
| D11 匹配 | 取第一个匹配而非最长；平手 Allow 不胜；`$` 不作锚；我们的组不优先于 `*` | `TestRobotsRulesFollowRFC9309`（各一次） |
| D11 缓存 | 一小时改为立即过期；不设条数上限 | `TestRobotsTxtIsCachedPerOriginForAnHourAndFailuresForAMinute`；`TestTheRobotsCacheIsBounded` |
| D11 512 KiB | 上限改为 64 MiB | `TestRobotsTxtIsReadUpToItsLimitAndNotRefusedForBeingLarger` |
| D12 长度界限 | 去掉 1000 字符界限；去掉 300 字符界限 | `TestLongRealArticlesThatMentionBotChecksAreNotBotChecks`、`TestTheBoundOnVisibleTextIsWhatKeepsAMarkerFromBeingAVerdict` |
| D12 每个标记 | 清单里 19 条各去掉一条 | 每次都是 `TestEachBotCheckMarkerMakesAShortPageABotCheck`（第一轮有两条没有失败，因为另一条标记也命中那一页；补了只含该标记的页面后各自失败） |
| D12 响应头与状态 | 忽略 `Cf-Mitigated`；不看 403、429、503 的正文；不对提取后的页面判断 | `TestAChallengeAnsweredWithAnErrorStatusOrAHeaderIsABotCheckNotAStatus`（各一次） |
| D13 证书 | `InsecureSkipVerify` | `TestTLSVerifiesTheHostNameAndTheChain` |
| D14 例外 | 例外扩到整个主机（解析处、拨号处各一次） | `TestATestOriginIsExemptAndNothingElseIs`；`TestTheCheckedDialRefusesWhatTheParserWouldHaveRefusedItself` |
| D14 配置解析 | 接受通配符 | `TestATestOriginThatCouldExemptMoreThanWasMeantIsAnError` |

没有单独失败的三处，都是有意的重复防线，不是漏洞：

- D6 里区检查、::/8 等 2000::/3 以外的 IPv6 表项：2000::/3 规则本身就拒绝它们（`Contains` 对带区地址为假，所以带区地址也落在规则之外），单独去掉其中一个看不出来；与 2000::/3 规则**同时**去掉才会失败（上表）。表项保留，因为它是登记表的记录，也因为 2000::/3 规则是 W3 新增、与沙箱不同。
- D9 的 `Content-Length` 预判：去掉后，按字节读到 5 MiB 加一字节仍会拒绝，预判只是省得去读。
- 重定向到私网字面量：解析器、拨号处两层各自拒绝，去掉一层仍被另一层拒绝；两层同时去掉时才失败（上表最后一条 D8）。

D13 的根证书本身不是单元测试能证明的：见任务报告里在镜像中的 `-check-tls`。

## 5. 其余

搜索、`ref`、租户配置：见平台 ADR-0003 与它的附录 C，由后续任务写入本 ADR。地址检查、读取、提取见 D5 到 D15。
