# ADR-0001　idp-web 的设计（骨架、读取、工具）

| 项 | 内容 |
|---|---|
| 状态 | 已接受（2026-11-19，随任务 W1；平台 ADR-0003 附录 C 的阶段 1 计划）；D16 到 D25 随任务 W4；D26 随任务 P2（Kubernetes 部署）；D27 随平台 ADR-0003 附录 D 第二阶段的任务 W4（需要脚本的页面）；D28 到 D30 随同一阶段的任务 W6（每日配额、PostgreSQL、来源编号入库）；D31 随同一阶段的任务 W5（PDF） |
| 范围 | `idp-web` 的技术栈、调用者校验、工具声明、正文提取的接口与规则、地址规则与读取（D6 到 D15，任务 W3）。`ref`、搜索适配器、租户配置、编号、缓存、限流、审计、错误句子（D16 到 D25，任务 W4）；每日配额、数据库与跨副本的编号（D28 到 D30，任务 W6）；PDF 的读取、解析库的选择与子进程的隔离（D31，任务 W5） |
| 依据 | 平台 ADR-0003（网页搜索与读取）D1、D2；idp-sandbox ADR-0001 D10、D18；idp-mcp ADR-0001 D40、D44；平台 ADR-0003 D4、7.3（T3、T7）、13.1；idp-sandbox ADR-0001 D22；RFC 9309；MCP 2026-07-28 |

## 1. 背景与边界

平台 ADR-0003 决定新增 idp-web：Agent 搜索网页、读取网页的上游服务，放在 idp-mcp 网关后面，与 idp-sandbox 并列。本 ADR 只记仓库自己的决策。为什么要有这个组件、外泄通道怎么切断、`ref` 和地址检查的设计，都在平台 ADR-0003，这里不重复。

本任务（W1）只建骨架：服务能启动、能校验调用者、能列出三个工具；工具本身不工作。

## 2. 决策

| # | 决策 | 理由 |
|---|---|---|
| D1 | **照 idp-sandbox 的方式搭：Go，MCP 手写，骨架阶段只用标准库；MCP 层从沙箱复制，不共享代码** | 沙箱 D10 已做过同样的选择：协议准入规则是 idp-mcp 网关的规则，两边各复制一份，比引入一个共享模块（要版本、要发布、要三个仓库同时升级）简单。复制会漂移，所以把漂移变成测试失败：`internal/mcp` 带着沙箱的同一组断言向量，由 idp-mcp 的签名器在固定时刻为固定请求签出，同一个请求和摘要也钉在 idp-mcp 的 `TestRequestDigestIsTheOneUpstreamsCompute` 里；哪一边改了规范化或校验规则，这条测试就会失败。向量里的受众（`http://sandbox:8570/mcp`）和工具名（`sandbox_run`）是被签名的内容，所以原样保留，不改成 web。后续任务的第三方依赖（HTML 解析、正文提取）在各自的决策里写明理由，CI 跑 govulncheck **补记（2026-11-19）**：`Mcp-Method` 请求头现在是必需的，通知也不例外。2026-07-28 的传输要求每个请求都带它，缺了就是 400 与 -32020（"Mcp-Method header is required"）；此前只在带了的时候才与请求体的方法比对（不一致仍是 400 与 -32020）。随 idp-mcp ADR-0001 D12（提交 a0ca6ce，网关改为要求该头）一起改；检查放在协议版本检查之后、通知返回之前，与网关一致：别的修订的客户端先被告知版本不支持，而不是缺一个本修订的头。后果：平台里不经网关、直接连本服务的检查（compose 的 e2e.sh 里 PATH D 与 PATH M 的伪造调用者检查，tests/kind/full.sh 的直连请求）必须带上这个头，否则得到的是 400 而不是它们期望的 403，这一改动要与平台的这几行在同一个版本里发布。本仓库的直连调用方（deploy/e2e.sh、测试）已加上。`internal/mcp` 的 `TestTheMethodHeaderIsRequiredAndMustMatchTheBody` 覆盖缺失、不一致与一致三种情形 |
| D2 | **调用者只取自网关签名的断言** | 设了 `IDP_WEB_CALLER_KEYS_FILE` 与 `IDP_WEB_CALLER_AUDIENCE` 之后，每个 `tools/call` 必须带 `_meta["io.idp.telos/caller-assertion"]`：ES256、`typ` 为 `idp-caller+jwt`、`kid` 是持有的公钥、签名验得过、`aud` 是本服务、没过期（允许 30 秒时钟差，有效期超过 5 分钟的不收）、不是未来签的、`req` 等于这次请求的摘要（参数与签名时不同即拒绝）、`jti` 只收一次；公钥必须是 P-256。没有断言得到 HTTP 403，工具不会运行；`tools/list` 与 `server/discover` 不需要调用者（网关的漂移检查没有）。租户和成员只来自校验过的调用者，不是工具参数：`ref` 要绑定调用者（ADR-0003 4.3），没有这一条它就没有意义。公钥与受众只设一个，启动即退出（状态码 2）。`IDP_WEB_REQUIRE_CALLER` 缺省为真。每一种拒绝在 `internal/mcp` 里是一个有名字的反例，并有一个正例在先，证明拒绝出于预期的原因。**一致性向量（2026-11-19）**：验签规则有一份与 idp-mcp、idp-sandbox 共用的判例：平台发布的一致性向量 `idp-contracts/vectors/caller-assertion/v1.json`（格式与用例见同目录 README），本仓库的副本是 `internal/mcp/testdata/caller-assertion-v1.json`，`caller_vectors_test.go` 让每个用例经过真实的 `CallerVerifier` 与服务器的 `tools/call`，结果必须与文件一致。向量由 idp-mcp 用真实的签名代码生成：`cd idp-mcp && go test ./internal/gateway -run TestCallerVectors -update`，再把同一份复制到四处（idp-contracts、idp-mcp、idp-sandbox、idp-web），四份必须一起改，`idp-contracts/tools/validate.py` 检查逐字节相同。上面「每一种拒绝在 `internal/mcp` 里是一个有名字的反例」现在指向这份向量里的用例；原来手写的那一组（`TestEachRefusalOfAnAssertionIsForItsOwnReason`）已删除，只留下对非 P-256 公钥的配置检查 |
| D3 | **三个工具的参数与标注先定下来，处理程序以后再换** | 网关在评审时钉住每个工具的 `inputSchema` 与标注，之后的任何变化都要重新评审（idp-mcp 的漂移封锁）。先把最终声明定下并放进测试，后面的任务只换处理程序，不会因改参数而反复评审。`web_search {query, freshness?, count?}`（`count` 为 1 到 10，`freshness` 取 `day`、`week`、`month`、`year`）；`web_fetch {url, ref, offset?}`；`web_open_url {url, offset?}`；都是 `additionalProperties: false`，标注都是 `readOnlyHint: true`、`openWorldHint: true`。参数严格解码，未知字段是错误。骨架阶段每个处理程序在校验参数之后返回固定的错误句子"web search is not available yet"，W4 换成真正的处理程序（D16 到 D24），声明的摘要由 `TestTheDeclaredSchemasAreTheOnesTheGatewayPinned` 钉住；错误句子是哨兵错误到消息的映射，其他错误只进日志，模型得到的是固定的一句 |
| D4 | **审计只记元数据，调用者记短哈希** | 每次调用一行日志：工具、结果、耗时、调用者（租户与成员的 SHA-256 前 4 字节）。不记成员标识本身，不记参数内容。查询词和地址的审计字段由 D23 加入（W4） |
| D5 | **正文提取：接口、纯文本读取器、回退规则、转 Markdown、隐藏内容规则已定；引擎用 readability（3.7，已由新页面集确认）** | 见下面 3.1 到 3.7。`internal/extract` 里有两个引擎：`readability`（缺省）和 `plain`。选择在 51 页真实网页和合成 E4 上量出来（平台 ADR-0003 附录 C 4.6、第 3 节的判据），再由另采的 30 页确认；落选的 go-trafilatura 和它的依赖已从代码里删除（它的实现在提交 7e094bb 里） |
| D6 | **哪些地址算公网：IANA 特殊用途登记表里"不可全球路由"的一律拒绝，不在表里的保留空间和带区的地址也拒绝** | 表写成 `internal/addr` 里的一张数据表，注明登记表与日期（2026-11-19），从 idp-sandbox 出口代理的表改写而来（两个仓库不共享代码，改一处要手工改另一处）：IPv4 的 14 块、IPv6 的 11 块，含 NAT64、6to4、Teredo 整块拒绝（它们能到达里面嵌的 IPv4 地址）；IPv4 映射的 IPv6 地址按它携带的 IPv4 地址判断；任何带区的地址一律拒绝（`netip.Prefix.Contains` 对带区地址恒为假，只查表会放行）。比沙箱多一条：2000::/3 之外的 IPv6 空间（登记表没有逐条列出的未分配空间）也拒绝，因为没有网站会在那里。相对 Python 参照 `is_global` 因此更严，差异只在这一段。测试：`TestEveryBlockOfTheRegistryTableIsNotPublicAtItsFirstMiddleAndLastAddress`（用表本身生成每块的首、中、尾地址）、`TestAddressesThatAreNotPublicAreNotPublic`、`TestEveryZonedAddressIsNotPublic`、`TestPublicAddressesNextToTheSpecialPurposeBlocksStayPublic`（相邻的公网地址仍放行，防止过严）、`TestEmbeddedIPv4ShowsWhatATranslatedAddressWouldReach` |
| D7 | **一个解析器，一次解析：`addr.Target` 返回之后的每一步只用它给的协议、主机、端口** | 两个解析器对同一段文字的理解不同，是 SSRF 的常见来源。`Target` 用 `net/url` 解析一次，拒绝：http 和 https 以外的协议、无 `//` 的地址、带用户名密码的地址（包括空的）、80 和 443 以外的端口、空主机、IDNA 转换失败的主机；国际化域名先转成 ASCII 形式；十进制、十六进制、八进制、短写的 IP（`2130706433`、`0x7f.1`、`0177.0.0.1`、`127.1`，判据是最后一个标签是数字，与 WHATWG 的 IPv4 判断相同）一律拒绝，因为 Go 自己的解析器把它们当域名，C 库和浏览器当 127.0.0.1；全角数字和句号经 IDNA 映射成的 IP 同样拒绝。**规范写法的 IP 字面量：公网的放行，非公网的在这里就拒绝**（不等连接）；IPv6 字面量必须是规范写法且不带区。请求的地址由 `Address.URL()` 从这几个部分重建，不再用原文，所以检查过的与请求的是同一个。测试：`TestAddressesAreRefusedForTheReasonTheyAreWrong`、`TestTheTwentyAddressesOfTheSSRFListAreRefused`（评测的 20 个）、`TestAddressesThatMayBeReadAreParsedOnceIntoTheirParts`、`TestTheParsedAddressRoundTripsThroughItsOwnURL`、模糊测试 `FuzzTarget`（不崩溃；接受的地址协议为 http/https、端口 80/443、无用户信息、主机是名字或公网规范 IP；60 秒结果见任务报告） |
| D8 | **钉住地址的连接：Go 自己的解析器，每个连接解析一次，任何一个答案不是公网就拒绝，只连检查过的那个 IP，不用代理** | `fetch.checkedDial` 是读取器唯一的建连方式：解析（`net.Resolver{PreferGo: true}`，可注入）、检查**全部**答案（参照实现同样；一个名字有一个私网答案，就是攻击者能控制的名字）、再用 IP 去连，不再解析名字，所以"检查时公网、连接时私网"的 DNS 重绑定到不了内网；TLS 仍由传输层对请求里的主机名验证证书。HTTP 客户端 `Proxy: nil`，不读代理环境变量：代理会自己解析、自己连接，对地址的检查就没有意义（平台计划第 2 节第 8 条；必须经出口代理的部署不在阶段 1）。每个请求一个连接（关闭 keep-alive），所以每个请求都重新解析、重新检查。拨号处把端口和字面量再检查一遍，不依赖解析器已拒绝过。返回给模型的错误不写解析出的地址（内网地址不能经错误泄露），只写日志。测试：`TestANameWithAPrivateAnswerAmongItsPublicOnesIsRefusedWithoutAConnection`、`TestRebindingTheConnectionGoesToTheCheckedAddressAndTheNameIsLookedUpOnce`（第一次答公网、再问就答 127.0.0.1；只查一次、只连检查过的地址）、`TestEveryConnectionIsResolvedOnceAndConnectedToWhatWasChecked`、`TestTheCheckedDialRefusesWhatTheParserWouldHaveRefusedItself`、`TestTheTwentyAddressesOfTheSSRFListAreRefusedWithoutAConnection`、`TestProxyEnvironmentVariablesAreIgnored` |
| D9 | **重定向与限额：每一跳重走同一套检查，最多 5 次；5 MiB（解码后同样）、15 秒、响应头 64 KiB** | 重定向由 `follow` 自己跟（客户端的跟随关掉），每一跳都过 `Target`、`Allow` 钩子、robots.txt 和钉住地址的连接；第 6 次重定向被拒绝，报告最终地址。15 秒是一次 `Read` 的总时间，连接、响应头、正文以及这次读取要取的 robots.txt 都在里面（比参照的"每个请求 15 秒"更严）。正文先看 `Content-Length`，再按 5 MiB 加一字节读，超过即拒绝。重定向到 `unblock.` 开头的主机按验证页处理（联邦公报）。`Allow func(host string) error` 对第一个地址和每次重定向调用，返回的错误原样给调用者（任务 W4 用它执行租户域名名单）。测试：`TestFiveRedirectsAreFollowedAndTheFinalAddressIsReported`、`TestAChainOfSixRedirectsIsRefused`、`TestARedirectToAPrivateAddressIsRefusedAndNeverFollowed`、`TestARelativeRedirectIsResolvedAgainstTheAddressItCameFrom`、`TestABodyOverFiveMiBIsRefusedWhetherOrNotItsLengthIsAnnounced`、`TestABodyThatArrivesTooSlowlyIsRefusedAtTheDeadline`、`TestAServerThatNeverAnswersIsRefusedAtTheDeadline`、`TestResponseHeadersAreBounded`、`TestTheAllowHookSeesTheFirstAddressAndEveryRedirectAndCanRefuseThem` |
| D10 | **类型、压缩与字符集：只读 HTML、XHTML、纯文本；gzip 与 deflate 自己解，解码后同样限 5 MiB；字符集解成 UTF-8** | PDF 单独报错（"这是 PDF，暂不支持"；**补记：D31 起 PDF 读取了，这一句和 `ErrPDF` 取消**），其他类型报"content type … is not read"，没有类型也拒绝（参照同）。自己发 `Accept-Encoding: gzip, deflate` 并关掉传输层的自动解压，上限才由我们定：解码器通过限长读取，永远不读到尽头，几 KB 的压缩炸弹展开不到 5 MiB 以上。deflate 先按 zlib 包装解，失败再按裸流解（参照同，真实网站有这样发的）。其他编码（br、zstd、叠加的 `gzip, br`）和解不开的数据拒绝。字符集按 Content-Type、字节序标记、文档 meta 的次序取（`x/net/html/charset`）；都没声明而字节是合法 UTF-8 时按 UTF-8（库对未声明页面的缺省是 windows-1252，会把不声明字符集的现代页面读成乱码）。新增依赖：`golang.org/x/text` 由间接变直接（BSD-3-Clause，字符集与测试里的 GBK 编码）。测试：`TestContentTypesThatAreNotTextAreRefusedWithTheirOwnMessages`、`TestOnlyGzipAndDeflateAreDecodedAndAnythingElseIsRefused`（含裸 deflate、损坏数据）、`TestACompressionBombIsRefusedByItsDecodedSize`、`TestPagesAreDecodedToUTF8ByTheirDeclaredCharset`（GBK 的头、meta 两种、无声明的 UTF-8、字节序标记） |
| D11 | **robots.txt 按 RFC 9309，不按参照实现；自己写，不加依赖** | 用户代理分组（我们的产品名 `idp-web` 与 `*`；有分组点名我们时只用点名的，各组合并），Allow/Disallow 取最长匹配、平手 Allow 胜、`*` 与 `$`、百分号编码按标准形式比较。经同一条钉住地址的路径取（起点与页面在同一来源，已过页面的检查；robots.txt 的重定向再过 `Allow`），最多读 512 KiB（超出的部分不读，不是整个拒绝，RFC 2.5 要求至少读前 500 KiB），按来源（协议、主机、端口）缓存一小时，最多 1024 个来源，满了先清过期的、再清最先到期的。**与 Python 参照的差别：** 参照对任何读不到的 robots.txt 都按"全部允许"；RFC 2.3.1.3 说 4xx 才是"没有限制"，2.3.1.4 说 5xx 或不可达要按"整站不可抓"处理。一个只在好读的时候才遵守 robots.txt 的读取器，站点没法信任它，所以按 RFC：4xx 放行，5xx、连不上、读不出都拒绝（错误说明是 robots.txt 读不到）；重定向超过 5 次按不可用处理，放行。读不到的结果只缓存一分钟，免得每个页面都去问一个坏掉的站点。若 robots.txt 的地址本身是不可连的（非公网），页面的真实错误是"地址不是公网"，原样返回。测试：`TestRobotsRulesFollowRFC9309`（34 条表驱动）、`TestRobotsTxtDecidesWhetherAPageIsRead`、`TestRobotsTxtOfFourHundredsMeansNoRestrictionsAndOfFiveHundredsOrNoAnswerMeansNoReading`（含 5xx、连接被切断、损坏的压缩）、`TestRobotsTxtIsFetchedThroughTheSameCheckedPath`、`TestRobotsTxtIsCachedPerOriginForAnHourAndFailuresForAMinute`、`TestTheRobotsCacheIsBounded`、`TestRobotsTxtIsReadUpToItsLimitAndNotRefusedForBeingLarger` |
| D12 | **验证页（反爬页）不是页面正文：已知标记且可见文字很少时报"该网站返回了验证页"** | 挑战页常以 200 返回，交给模型它会被当成页面来总结，比报错更糟。标记清单只在 `internal/fetch/botcheck.go` 一处，每条写明在哪里见过：Cloudflare（`_cf_chl_opt`、`cf-chl-`、`cf-browser-verification`、标题"Just a moment..."、"Attention Required! \| Cloudflare"，响应头 `Cf-Mitigated: challenge`）、AWS WAF（`awsWafCookieDomainList`、`gokuProps`；eur-lex 以 200 返回 2035 字节的挑战页）、联邦公报（重定向到 `unblock.` 主机）、字节与火山引擎（标题"Please wait..."、"正在进行安全检测"）、以及可见文字在 300 字以内、内容就是验证码提示的短页（"verify you are human"、"captcha"、"人机验证"等）。**误报的防线是可见文字的长度**：有标记的页面，可见文字超过 1000 字符就是页面（真实文章会提到验证码，许多 Cloudflare 站点每页都带检测脚本）；提示类文字再收紧到 300 字符。可见文字取提取之后的正文，即模型本会拿到的文字。403、429、503 的答案也看正文（挑战页常这样返回），否则"HTTP 403"掩盖了真实原因。测试：`TestEachBotCheckMarkerMakesAShortPageABotCheck`（每个标记一页）、`TestLongRealArticlesThatMentionBotChecksAreNotBotChecks`（长文章提到验证码、带检测脚本、标题恰为"Just a moment..."）、`TestTheBoundOnVisibleTextIsWhatKeepsAMarkerFromBeingAVerdict`、`TestAChallengeAnsweredWithAnErrorStatusOrAHeaderIsABotCheckNotAStatus`、`TestARedirectToAnUnblockHostIsABotCheck` |
| D13 | **TLS 根证书：用 `golang.org/x/crypto/x509roots/fallback`，在 `cmd/idp-web` 里为副作用导入** | 镜像是 `FROM scratch`，没有 CA 证书，HTTPS 会失败。两条路里选这一条：不用往镜像里复制文件（`deploy/build.sh` 不依赖主机的证书目录，镜像仍只有一个文件），根证书随 Go 模块版本固定，进入 `go.sum` 和 govulncheck 的视野，构建仍可离线重现；系统有根证书时 Go 优先用系统的，所以在有证书的环境里行为不变。更新方式：`go get golang.org/x/crypto/x509roots/fallback@latest`，重建镜像；包是从 Mozilla NSS 根存储生成的，当前版本 v0.0.0-20261115193255-739070949446（2026-11-15），许可证 BSD-3-Clause（Go 作者；证书数据来自 NSS）。跟不上 NSS 的撤销是这条路的代价，所以要随依赖更新（CI 已跑 govulncheck，另应按月更新）。证明：不是单元测试，而是在镜像里跑 `idp-web -check-tls https://example.org/`（任务报告里有输出，过期证书的站点被拒绝）；`TestTLSVerifiesTheHostNameAndTheChain` 另证主机名与证书链是被验证的 |
| D14 | **测试用例外：`IDP_WEB_TEST_ORIGINS` 只豁免列出的 `主机:端口`，其余一概不豁免** | 逗号分隔的精确 `主机:端口`，在 `cmd/idp-web` 解析（通配符、缺端口、空主机、带区的地址都是错误：松散的条目豁免得比本意多），传给读取器。只豁免端口规则与公网规则；用户名密码、协议、非规范写法仍然拒绝；同一主机的另一个端口、同一端口的另一个主机、重定向到别处都不豁免。服务启动时若设了它，日志警告。清单里没有它由 D26 的 `tests/deploy` 断言。测试：`TestATestOriginIsExemptAndNothingElseIs`、`TestTestOriginsExemptExactlyTheListedHostAndPortAndNothingElse`、`TestTestOriginsAreParsedIntoTheFormTheReaderComparesWith`、`TestATestOriginThatCouldExemptMoreThanWasMeantIsAnError` |
| D15 | **错误是模型能据以行动的小写句子，内部细节只进日志** | `ErrNotPublic`（`addr.ErrNotPublic`，字面量与解析结果同一个）、`ErrRobots`、`ErrTooLarge`、`ErrTooSlow`、`ErrType`、`ErrEncoding`（`ErrPDF` 已由 D31 的七个 PDF 错误取代：`ErrPDFNoText`、`ErrPDFEncrypted`、`ErrPDFMalformed`、`ErrPDFTooLarge`、`ErrPDFTooSlow`、`ErrPDFUnavailable`、`ErrPDFBusy`，即 `internal/pdfread` 里同一些值）、`ErrBotCheck`、`ErrStatus`（附状态码）、`ErrTooManyRedirects`、`ErrConnect`、`ErrUnreadable`，加 `addr.ErrRefused`（地址形式，附原因）；用 `errors.Is` 判断。连接错误、解析出的地址、传输层的原文不进错误，只进日志。调用者取消时返回 `context.Canceled`，不当作超时。测试：各条错误对应的测试同上；`TestTheCallersCancellationIsReturnedAsItIs`、`TestAConnectionThatCannotBeMadeIsReportedWithoutItsInternals` |
| D16 | **`ref`：25 个字符，HMAC-SHA-256 对（地址、调用者、过期时间），三部分用带长度的编码拼接** | 形式与阶段 0 测过的相同：`<过期时间，8 位十六进制>.<MAC 的 16 个 base32 小写字符>`（80 位）。拼接：固定标签 `idp-web ref v1\0`，然后地址、调用者各带 4 字节大端长度，最后过期时间 4 字节大端，所以两个不同的三元组不会得到同一串字节（参照实现 `refs.py` 用 NUL 分隔，地址里有 NUL 时两个不同的三元组会撞；因此本实现的 `ref` 与 `refs.py` 的**不互通**，评测框架不拿它验证生产的 `ref`）。调用者是已验证调用者的 `租户/主体`，有效期 1 小时。密钥来自 `IDP_WEB_REF_KEY_FILE`：一行一把、每把至少 32 字节、第一把签发、全部验证（轮换：新密钥放前面，一小时后去掉旧的）。MAC 用常量时间比较（`subtle.ConstantTimeCompare`，所有密钥都比一遍）。`Check` 先验 MAC 再看过期：伪造的 `ref` 得不到"已过期"这类信息，只有本来真实的 `ref` 才会被报告过期。三个原因各是一个哨兵错误：不是 `ref`（`ErrMalformed`）、不是为这个地址和调用者做的（`ErrNotForIt`）、已过期（`ErrExpired`），模型读到的句子见 D24。web_fetch 要求地址与列出的写法逐字相同（列出的是规范化后的写法，D20）：多一个查询串就是另一个地址。每个控制的测试见第 5 节 |
| D17 | **搜索适配器：`Provider.Search(ctx, Query)`；发给服务商的只有 `Query` 的字段，没有调用者的任何标识** | `Query{Text, Freshness, Count, IncludeDomains, ExcludeDomains}`。`exa`：`POST {base}/search`，`x-api-key`，`type`（`IDP_WEB_EXA_TYPE`，`auto` 缺省或 `lite`）、`numResults`、`contents:{highlights:true}`、由 `freshness` 算出的 `startPublishedDate`、包含与排除域名；基址 `IDP_WEB_EXA_BASE_URL`（缺省 `https://api.exa.ai`，欧盟 `https://api.eu.exa.ai`，必须是 https，因为 API 密钥会发到那里）。`brave`：`GET …/res/v1/web/search?q&count&extra_snippets=true[&freshness=pd|pw|pm|py]`，`X-Subscription-Token`；Brave 没有域名参数，名单写进查询词（`-site:` 每个排除域名一个；包含域名只有一个时写 `site:`，有几个时不写：参照实现只取第一个，会把其余的域名挡在结果外，不写则由 D20 的检查筛掉不在名单里的结果，代价是结果可能变少）。`fixture`：`IDP_WEB_FIXTURE_FILE`，YAML：`entries: [{match, results: [{url, title, published, snippet}]}]` 与可选的 `default`；`match` 是查询词的子串（不分大小写，先匹配的先用）；不联网、忽略域名名单与新鲜度（像一个过滤不生效的服务商，使名单的强制必须由调用方做到，D20）。服务商的 HTTP 客户端与页面读取器**分开**：API 主机是固定的可信目的地，所以走环境里的代理变量（启动时读一次，规则同 `http.ProxyFromEnvironment`；读取器则忽略代理，D8）；自己的 10 秒超时；应答最多 4 MiB（读完再看，使限制是限制，不取决于 JSON 解码器读到哪里）；只跟随同主机、同协议的重定向，不跟随到另一个主机（密钥是请求头）。服务商返回的标题和摘录压成一行并截断（含换行的标题可以写出一行像结果列表自己的 `ref:` 或 `[9]`）。服务商的错误详情（状态、应答正文、网络错误）写日志，模型只读到固定的一句（`ErrUnavailable`、`ErrBusy`，D24） |
| D18 | **租户文件：`IDP_WEB_TENANTS_FILE`，启动时读一次；租户只取自已验证的调用者** | `default` 与 `tenants: {<租户号>: {enabled, includeDomains, excludeDomains, openUrl}}`；未知的键是错误（拼错的 `excludeDomain` 不能悄悄让一个域名可读）；没有列出的租户用 `default`，没有 `default` 时一律不可用；`openUrl` 缺省为关。域名项匹配该名字本身与它的子域名，不匹配 `notexample.com` 这样的近似名；含协议、路径、端口、通配符的项是错误（不猜）；国际化名字按 ASCII（punycode）形式比较。排除优先于包含；包含名单为空表示所有域名。名单在两处强制，不依赖服务商（有报告称某些服务商的屏蔽名单有时不生效，平台 ADR-0003 D6）：搜索结果在给出 `ref` 之前（D20），读取时第一个地址与每一次重定向（D20）。`Policy.AllowHost` 是唯一的判断 |
| D19 | **编号：`n` 在一个调用者名下标识一个来源，所以一次运行里几次搜索之后回答中的 `[n]` 不会有歧义** | 内存里按调用者保存"地址 → 第一次给出的号"，新地址取下一个空号；号不重用（一个号从不指向两个地址）。一小时没用就忘掉；调用者数（1 万）与每个调用者的地址数（1000）有上限，超了先丢最久没用的地址或调用者。读取页面给出的引用用同一张表，所以 `web_fetch` 的 `citations[0].n` 与搜索里列出它的那一项相同。**这是按副本的**：阶段 1 只跑一个副本；多副本或重启后编号会分叉，阶段 2 把编号移到 HOST 的 `sources`（平台 ADR-0003 D9）。限制：编号表忘掉某个地址后再见到它会得到新号，模型上下文里旧号仍指向同一地址，所以仍然无歧义 **补记（任务 W6）：**上面"这是按副本的……阶段 2 把编号移到 HOST 的 `sources`"已被 D29 取代：配置了数据库时，编号在 PostgreSQL 里，同一个调用者的同一个地址在所有副本上、重启之后都是同一个号，寿命与上限的规则不变（一小时没用即忘、每个调用者 1000 个地址、号不重用）；没有配置数据库时仍是这里写的内存行为，只能单副本。调用者数的上限（1 万）只属于内存实现；库里由清理删除空闲的调用者。 **补记（跟进）：**号标识的是**被要求的地址**，所以读取给出的引用也用被要求的地址（`url`），即使读取在重定向之后落在别处；运行时按号记录来源、同一个号第二个不同地址记为冲突（HOST D49），如果引用换成最终地址，同一个页面的"搜索 → 读取"就会被数成冲突。最终地址在另一个可选字段里（D20）。 |
| D20 | **工具的流程与结果格式** | `web_search`：校验参数 → 租户已启用 → 限流 → 服务商 → 每条结果过 `addr.TargetWith`（含测试例外）与租户名单，不过的丢掉并在审计行里计数，**不带 `ref` 的结果从不返回**；地址写成规范形式（小写主机、去片段与默认端口），重复的只留一条；每条是 `[n] 标题` / `address:` / `ref:` / `published:` / `excerpt:`（与 `tools/web_eval/agent.py` 相同，条与条之间空一行，没有结果写 `no results`）；`structuredContent.citations = [{n, title, url}]`。`web_fetch`：参数 → 租户已启用 → **`ref` 校验（T1、T2：不是搜索给出的地址就读不了）** → 地址规则与租户名单（第一个地址）→ 缓存 → 限流 → `Reader.Read` → `extract.Page`（12 KiB 一段）；首行 `[n] 标题 (bytes a–b of 总长; the rest: web_fetch with offset N)`，与参照实现相同（`web_open_url` 里写 `web_open_url`）；段内没有文字写 `(no text found on this page)`；`citations = [{n, title, url（被要求的地址，即编号的那个，与搜索结果里的相同）, finalUrl（只在读取被重定向到别处时出现，是最终地址，再过一次 `addr.TargetWith`）, fetchedAt, sha256（这一段文字的）}]`。`web_open_url`：只在租户 `openUrl: true` 时可用，不要 `ref`，其余同上；关闭时的句子写明管理员要在租户文件里改什么。每个重定向的域名检查走 `fetch.WithAllow`：读取器是所有租户共用的一个，`Config.Allow` 在构造时固定，租户的规则属于这一次调用，所以放在调用的 context 里（对 `internal/fetch` 的唯一改动，`TestAnAllowRuleOfTheCallSeesEveryAddressAndCanRefuseARedirect`）；第一个地址在读取器之前也检查一次，这样被拒的读取不占读取配额（`TestARefusedReadDoesNotUseTheReadQuota`）。与参照实现的差别：错误不加 `error:` 前缀（`isError: true` 已经表示），也不加 `<<untrusted web content>>` 的外壳（那是 HOST 的 `wrap`，平台 ADR-0003 D8）；标题为空时用地址；重定向后首行在标题后加 ` [the address redirected to <最终地址>]`（在 `(bytes …` 之前，评测的首行正则仍然匹配），让模型看得见；缓存按被要求的地址存，引用的 `url` 与 `finalUrl` 在缓存命中时相同（`TestAReadThatRedirectsIsCitedUnderTheAddressThatWasAskedForAndSaysWhereItEnded`、`TestAReadWithoutARedirectHasNoFinalAddress`、`TestAReadThatRedirectsToAnAddressTheRulesRefuseIsStillRefused`） |
| D21 | **读取缓存：按租户、以请求的地址为键、一小时、有条数与字节上限；搜索结果不缓存** | 为了后一段（`offset`）不再请求源站。缺省 256 页、64 MiB（解码后的文字），最久没用的先丢；比整个缓存还大的页面不缓存。租户之间不共享（一个租户读过的页面不会给另一个租户）。缓存里的页面带原来的读取时间，`fetchedAt` 就是它。搜索结果从不进缓存（服务商条款通常限制存储）。命中缓存也要先过 `ref` 与名单检查 |
| D22 | **限流：按调用者的滑动窗口，内存里，按副本** | 缺省 10 分钟 30 次搜索、30 次读取，`IDP_WEB_LIMIT_SEARCHES`、`IDP_WEB_LIMIT_READS`、`IDP_WEB_LIMIT_WINDOW_S`。读取只数真正发往网站的（缓存命中的后几段不数，被规则拒绝的也不数）；超限的调用不记入，告诉模型多少秒后可再试，并让它用已有的内容回答。每次运行的上限做不到（调用者断言里没有运行号，附录 C 第 2 节第 4 条），HOST 的步数限制兜底；已知差距。窗口表有大小上限 **补记（任务 W6）：**滑动窗口仍在内存里、按副本，不入库：它防的是滥用，不是配额；N 个副本时，一个调用者在窗口内最多可以做 N 倍的调用，**每日配额（D28）才是精确的上限**。 |
| D23 | **审计：每次调用一行 JSON（`msg":"audit"`）** | 字段：时间、`tool`、`outcome`、`ms`、`tenant`、`caller`（租户与主体的 SHA-256 前 4 字节）、`query`（明文：它就是外发的内容）、`url`（明文，包括被拒绝的：模型写了什么地址正是要看的）、`results`、`dropped`、`bytes`（交给模型的）、`cache`、`decision`（`allowed`、`refused`、`failed`）、`reason`（`ok` 或 D24 的原因名）。**没有 `ref`、密钥、主体本身和页面文字。**留存期是部署的日志策略，不在本服务里。`TestTheAuditLineHasTheQueryAndTheAddressAndNeitherTheRefNorAKeyNorPageText` |
| D24 | **错误句子、指标、就绪与配置** | 每个哨兵（`fetch`、`addr`、`refs`、`tenants`、`search` 与本包的）在 `internal/tools/errors.go` 的 `mappings` 里有一行：原因名、决定（`refused` 规则拒绝；`failed` 网站或服务商没给出）与模型读到的句子；其中 `ref` 的句子是"那个地址不是搜索结果里的：先搜索，再原样使用列出的地址与 ref"。不在表里的错误：详情写日志（`tool failed`），模型读到"the web tool failed to complete the request"。`TestEverySentinelHasAFixedSentence` 列出所有哨兵，加了哨兵而没加这一行就失败。指标新增 `idp_web_refused_total{tool,reason}`（被规则拒绝的调用，按原因）。`IDP_WEB_PROVIDER`（`fixture`、`exa`、`brave`）必须设置，它的密钥或文件缺失，`IDP_WEB_REF_KEY_FILE`、`IDP_WEB_TENANTS_FILE` 缺失或无效，都是配置错误，退出码 2。`/readyz` 只有在服务商、`ref` 密钥与租户文件都已载入时才是 200，否则 503 并写明缺什么。密钥一律来自文件，不来自环境变量 |
| D25 | **新增依赖：`gopkg.in/yaml.v3` v3.0.1（许可证 MIT 与 Apache-2.0；它的 libyaml 移植部分是 MIT，其余是 Apache-2.0）** | 租户文件与 fixture 文件是 YAML（平台 ADR-0003 附录 C 4.7），标准库没有 YAML。idp-sandbox 已用同一版本。只用 `Decoder` 与 `KnownFields(true)`。除它之外没有新增依赖；`golang.org/x/net/http/httpproxy` 与 `idna` 属于已有的 `golang.org/x/net`。`govulncheck ./...` 无漏洞 |
| D26 | **Kubernetes 部署：`deploy/kubernetes`，一个副本，发布的清单是生产形状，验收要的东西全在平台的覆盖层里** | 命名空间 `idp-web`（restricted，带 `idp-mcp/mcp-upstream` 标签），一个 Deployment（副本数 1、策略 `Recreate`：编号、缓存与限流是按副本的内存状态，D19、D21、D22，两个 Pod 会给同一调用者两套编号和两份额度；非 root、只读根文件系统、不挂服务账号令牌、探针在运维端口）、Service（8580）、NetworkPolicy。**租户文件**随清单发布，内容是 `default: {enabled: false}`：没有运维明说，谁也不能用（附录 C 4.7）；它由 kustomize 的 `configMapGenerator` 生成，名字随内容变，改了就换 Pod，正好对应"启动时读一次"。**密钥**（`ref` 密钥 `idp-web-ref-key`、搜索服务的密钥 `idp-web-search-key`）与**网关公钥**（ConfigMap `idp-web-gateway-caller-keys`，它不是秘密）由运维创建，清单里只写名字。**调用者校验默认开**：清单设了 `IDP_WEB_CALLER_KEYS_FILE` 与 `IDP_WEB_CALLER_AUDIENCE=http://idp-web.idp-web.svc.cluster.local:8580/mcp`（Service 的 URL，要与网关里登记的端点逐字相同）。**搜索服务不设默认**：选哪家就是决定谁看到 Agent 的每一个查询、在哪个地区、按谁的条款，这是运维的决定，不该由清单替他做；没设时服务启动即退出（2，D24），Pod 不就绪，没有悄悄用 fixture 的可能。清单里不出现 `IDP_WEB_TEST_ORIGINS` 与 fixture，`tests/deploy` 断言这一点（D14）。**网络**：默认拒绝；DNS；入口 8580 只放网关与注册服务两个 Pod（注册服务要发现工具，没有它登记不了，所以放在组件自己的清单里，而不是像沙箱那样留给平台覆盖层；它能列工具、不能调用：没有网关签名的 `tools/call` 由服务自己拒绝 403）；运维端口只放 `observability` 命名空间；出口只到公网地址的 443 与 80：`0.0.0.0/0` 减去 `internal/addr` 表里的 IPv4 部分，**只有 IPv4**（没有 IPv6 规则就到不了任何 IPv6 地址，与沙箱出口代理相同；要 IPv6 的集群自己加 `::/0` 并减去 `fc00::/7`、`fe80::/10`、`ff00::/8` 与自己的网段）。这是集群对服务自己那道拒绝的再做一遍：服务的 Bug 或配置错误不会打开集群或运维的网络。`tests/deploy/egress_test.go` 对整个 IPv4 空间逐个 /24 比较 `addr.Public` 与策略的 `except`，两边必须一致。集群内的测试源站在私网地址，发布的出口规则**正确地**到不了它们：验收（平台的 `tests/kind/full.sh`）自己加一条测试策略，这条策略和测试源站、`IDP_WEB_TEST_ORIGINS`、fixture 都只在平台的测试文件里。 | 沙箱 ADR-0001 D8、D22 的做法：策略是边界之一，策略与代码的两张表用测试对齐。把"选服务商"留给运维是因为附录 C 4.8 要求缺服务商时不启动；发布一个默认值就等于替所有部署做了这个选择。`tests/deploy` 里的测试：`TestShippedTenantsFileEnablesNobody`、`TestNothingTestOnlyIsShipped`、`TestEgressPolicyIsTheOnlyWayOutAndIsIPv4Only`、`TestServiceIsHardenedAndItsSettingsAgreeWithItsMounts` 等；`make test` 的 `go test ./...` 包含它们。CNI 是否执行这些策略，由平台 kind 验收的 PATH M 与 N 在 Calico 上证明 **补记（任务 W6）：**清单现在配置了数据库（Secret `idp-web-database`、迁移的 Job `idp-web-migrate`、到 PostgreSQL 的出口策略 `web-database`），所以副本数可以大于 1，策略改为 `RollingUpdate`（新旧 Pod 同时存在不再有害），`replicas` 仍写 1，由运维调高；上面"两个 Pod 会给同一调用者两套编号和两份额度"对配置了数据库的部署不再成立（编号与每日配额在库里；缓存和滑动窗口仍是各 Pod 的，D22）。不配数据库的部署（清单之外）仍必须是一个副本与 `Recreate`。出口策略由两条变为三条（`allow-dns`、`web-egress`、`web-database`），`TestEgressPolicyIsTheOnlyWayOutAndIsIPv4Only` 与 `TestTheDatabaseIsTheOnlyPlaceInTheClusterTheServiceAndTheJobMayReach` 各管一部分。 |
| D27 | **正文由脚本生成的页面不是页面：提取出的文字很少、且有脚本生成内容的正向特征时，报"这个页面的正文由脚本生成，现在读不了"，原因记为 `needs_script`** | 与验证页（D12）同一类问题：只用普通读取器读，这样的页面只剩站点的导航、页脚和一个空框，交给模型它会当作页面来总结，说"页面没有内容"。**两个条件同时成立才报**：（一）提取之后的可见文字不超过 1000 字节（取回退之后的文字，即模型本会拿到的）；（二）有下列五条特征之一。**只有文字少不够**：短通知、桩页面、错误页、跳转页都是正常结果，原样交给模型。五条特征各是 `internal/fetch/needsscript.go` 里一条有名字、各自有测试的规则：`empty_mount`（单页应用的框架页：`#root`、`#app`、`#__next`、`#__nuxt`、`#___gatsby`、`#q-app`、`#svelte` 或 `<app-root>`、`<ion-app>` 里没有任何文字、也没有图片或表格；转圈图标不算内容）；`placeholder_mount`（对 `empty_mount` 的放宽：挂载点里唯一的文字是一行加载提示，如 "loading..."、"正在加载"、"please wait"，至多 60 字节且整段就是这句话；菜鸟网络的首页就是这样：`#root` 里一行 "loading..."，框外一个站点地图链接）；`skeleton`（骨架占位：至少 3 个 class 含 `skeleton` 的元素，如 `xitu-skeleton`、`ant-skeleton`、`MuiSkeleton-root`，在本该是正文或列表的地方；掘金首页就是菜单和标签加一排骨架块，没有信息流）；`noscript_notice`（`noscript` 里写着需要或已关闭 JavaScript，如 create-react-app 的"You need to enable JavaScript to run this app"、"请开启 JavaScript"，并且页面确实从文件加载了脚本；一个静态页面里为评论组件写的提示不算）；`script_prose`（脚本里有一个至少 600 字节的字符串，是成段的文字而不是代码或 base64，并且这段文字**不在**页面显示的文字里：CMS 把正文放在变量里再写进空框，或数据块里带着正文由前端渲染。第二个条件让服务端渲染的应用不被误报：Next.js 的数据、Nuxt 的状态里的正文同时也在 HTML 里；JSON-LD 不执行，不算）。**阈值用字节而不是字符**：一个汉字三个字节，说的话与三个拉丁字母相当，按字符数会让中文页面的门槛比英文高三倍（500 个字符对中文是一大段，对英文只是一小段）；1000 字节约等于 170 个英文词或 330 个汉字。选 1000 而不是计划里的 500：站点框架（菜单、页脚、"加载中"）只有几百字节，数据里最短的真实页面是 590 到 920 字节（发言人答问、快递公告），每条规则又各要一个特征，所以文字上限只需把"框架"和"有正文的页面"分开；数据只给出一个上界：第一组里不加文字上限时，规则会报 5 页（4 页 `noscript_notice`，文字 1347 到 20728 字节，都是有正文的真实页面；1 页 `script_prose`，文字 11536 字节，原因没有查），所以上限必须低于 1347；加上上限后只剩央视页，上限取 500 到 1300 之间的任何值结果相同。1000 是在这个范围里按设计选的，不是量出来的，写在这里以免被当成量出来的。**与验证页的顺序**：先问验证页（D12），它保留自己的原因：挑战页也短、也满是脚本、也常带空框和 noscript 提示，但模型该知道的是"网站把我们当成了机器人"，两者的对策不同；`TestAChallengePageKeepsItsOwnReasonWhateverElseItShows`。**与回退（D5）**：两者都用回退之后的文字：回退救回的页面文字长，所以不会被报；框架页的回退结果只是菜单，仍短，所以不会被回退盖住。`TestTheFallbackNeitherMasksTheDetectionNorIsOverruledByIt`。**行为**：`web_fetch` 与 `web_open_url` 返回"the content of this page is produced by script and cannot be read now: use another source, or the excerpt of the search result"，审计 `decision=failed`、`reason=needs_script`，新指标 `idp_web_failed_total{tool,reason}` 单独计数（此前只有被拒绝的有计数）；日志里一行 `the page needs script` 带规则名。**测量**（`TestMeasureNeedsScriptOnLocalPages`，环境变量 `IDP_WEB_PAGES_DIR`，不设则跳过；页面不在仓库里）：第一组 52 页（规则据此定出）报 1 页，是已知的央视网新闻页（`e4r-zgov-08`，正文在 `<script>` 的字符串 `var contentdate = '<p>…'` 里，可见文字 352 字节），规则 `script_prose`，其余 51 页误报 0；另采的 30 页（留作确认）只跑一次，报 0 页，误报 0（其中没有需要脚本的页面，所以它证明不了召回）。**第三组：审阅者挑的十个站点**（`.local/web-eval/e4-shells`，因为预期是框架页才挑的，不是随机样本；页面同样不在仓库里）：十个里 1 个是验证页（36kr，火山引擎的"正在进行安全检测"，仍是 `bot_check`），4 个是框架页，5 个有正文。框架页抓到 3 个：`oschina-home`（`#app` 为空，0 字节，`empty_mount`）、`cainiao`（19 字节，挂载点里 "loading..."，`placeholder_mount`，加上第二条放宽才抓到）、`juejin`（428 字节，菜单加骨架块，`skeleton`，同上）；**漏掉 1 个**：`jdl-home`（京东物流首页，1152 字节，全是服务端渲染出的导航菜单，没有正文；有 `noscript` 提示，所以 `noscript_notice` 本该抓到它，但文字比 1000 字节的上限多 152 字节）。有正文的五个都没报：`oschina-news` 5108、`zto` 4757、`react-dev` 12836、`sf` 1197（顺丰新闻首页：十条带日期的标题，判为有短正文的真实页面）、`yto` 1498（圆通首页：各类服务的说明文字，判为真实页面）。即：**在审阅者挑的、预期是框架页的十个站点里，4 个框架页抓到 3 个**；这不是对整个网络的召回率，样本小，且 2 条新规则是看了其中 2 个页面之后加的，所以这 3 个不是独立的确认。**文字上限不为 `jdl-home` 抬高**：要抓到它上限得至少 1153，而 `sf`（真实页面）1197 字节时会被 `script_prose` 报出来，所以只有 1153 到 1196 这 44 字节的窗口，那是在凑一个页面；不加上限时第一组里最短的被报的真实页面是 `e4r-egov-11`（1347 字节，`noscript_notice`），另有 `e4r-zbiz-12`（11227 字节，`skeleton`）与另采组的 `e4v2-en-05`、`e4v2-en-07`，所以上限抬高的代价在 81 页上要到 1347 才出现，但到 1197 就在样本里报错一页。三组重跑：第一组 52 页报 1（央视页），另采 30 页报 0，现在另采组**已经看过**，从此是回归集，不再是确认。**要先说明的一点**：定规则之前，我对两组页面都打印过提取文字的长度与脚本字节数（没有跑任何规则），并且用一个临时脚本看过 `e4v2-zh-12` 是否被回退救回；规则是只在第一组上定的，规则成形之后才在第二组上跑了一次，没有因为第二组改过任何东西，但"没看过"不成立。**没有的数据**：第一组与另采组里没有一页带空挂载点，`empty_mount` 来自对常见框架的了解与合成页面的测试；`placeholder_mount` 与 `skeleton` 是看了第三组之后各依一个页面加的，各只有一个真实页面作证据，其余是合成页面；`noscript_notice` 在第一组有 4 页带这样的提示（都是有正文的真实页面，文字远超 1000 字节），文字上限就是它们的防线。**考虑过又放弃的规则**：脚本字节多（或出现 `self.__next_f`、`__NUXT__` 一类的数据标记）且文字少。第一组的顺丰快递公告（`e4r-zbiz-09`，Next.js，128 KB 的服务端渲染数据，可见文字 791 字节，正文完整）会被它误报：数据块大不说明正文不在 HTML 里。**不识别的**：框架页带有大量模板文字（超过 1000 字节的菜单、推荐、页脚），文字上限放过了它（`jdl-home` 就是：1152 字节的导航，漏掉了）；数据在页面加载之后另发请求取回的页面（框架页里没有文字、没有挂载点，也没有正文可比，只有空框）；挂载点不叫上述名字、也没有提示的框架页；正文在 `iframe` 或图片里；JSON 里不是成段文字的内容（表格数据、商品列表）。这些页面会作为短页原样交给模型。**用途**：`idp_web_failed_total{tool="web_fetch",reason="needs_script"}` 占 `web_fetch` 调用的份额，就是平台 ADR-0003 附录 D 第 6 节"渲染器做不做"的决策数据（门槛：占 5% 以上）；因为上面这些"不识别的"，**这个数是下限**：框架页漏报时，模型拿到的是菜单，计数里没有它。测试：每条规则一个测试（`TestAnEmptyMountPoint…`、`TestANoscriptNotice…`、`TestAnArticleThatOnlyExistsInAScriptString…`）、`TestTextInAScriptThatThePageAlsoShowsIsAPageNotAFrame`、`TestShortButNormalPagesAreNotFlagged`、`TestAMountPointThatOnlyHoldsALoadingLineIsAPageThatNeedsScript`、`TestAMountPointWithRealTextNextToALoadingWordIsAPage`、`TestPlaceholderBlocksInPlaceOfTheContentMakeAPageThatNeedsScript`、`TestOneStrayPlaceholderOrALongPageWithPlaceholdersIsAPage`、`TestAPageWhoseLongTextIsInATableIsNotFlagged`、`TestTheBoundOnVisibleTextIsInBytes…`、`TestAMountPointThatHoldsTheServerRenderedPageIsAPage`、`TestANoscriptNoticeIsNotEnough…`、`TestAPageThatNeedsScriptIsTheModelsSentence…`（工具层）、`TestTheFailedCounterIsWritten…` |
| D28 | **每日配额：租户文件的 `dailySearches` 与 `dailyReads`，按租户、按 UTC 日，计数在 PostgreSQL，"还有没有、并扣一次"是一条语句；数据库不可用时拒绝调用，不放行** | **设置。**`dailySearches`、`dailyReads`，写在 `default` 或租户条目里；租户条目整体替换 `default`，不与它合并，与其余字段相同（所以条目里没写的限额就是不限，即使 `default` 写了）。**不写或写 `~`：不限，且这类调用完全不碰数据库。`0`：一次都不允许**（可以让一个租户只能读成员给的地址、不能搜索；"不限"靠不写表达，不靠一个大数）。负数、不是整数（yaml.v3 会把 `1.5` 悄悄读成 `1`，所以用自己的类型拒绝它）、大于 10 亿（`MaxDaily`，列是 32 位整数）、字符串，都是租户文件的错误，服务启动即退出（2），错误写明键和范围。**一天是 UTC 日历日**；计数的键是 (租户号, 日期, 类别)，租户号是网关转发的、从不取自参数（D18）；用 `default` 的租户各有各的计数，不共享。**什么算一次：**搜索，在调用服务商时算，服务商答什么都算（错误、超时、被取消的调用也是发出去了）；读取，在真正发往网站时算：缓存命中的后几段不算（D21），被地址规则或租户名单在发出前拒绝的不算（D20）。**顺序：**参数检查 → 租户已启用 → `ref` 检查（只有 `web_fetch`）→ 第一个地址的规则与名单 → 缓存（命中则不再往下）→ 滑动窗口（D22）→ **取一次配额** → 外发调用。配额是最后一道检查，紧贴外发；它前面任何一道拒绝都不占配额，`TestACallThatIsRefusedBeforeItGoesOutDoesNotTouchTheQuota`。被配额拒绝（或查不了配额）的调用已在滑动窗口里占了一格，窗口在内存里不退还：配额用完之后的重试本来就都会被拒，这一格只多占窗口。**判定（`internal/state` 的 `takeSQL`，一条语句，不读后写）：**`INSERT INTO daily_usage (tenant, day, kind, used) SELECT $1, $2, $3, 1 WHERE $4 > 0 ON CONFLICT (tenant, day, kind) DO UPDATE SET used = daily_usage.used + 1 WHERE daily_usage.used < $4 RETURNING used`。有返回行就是取到了；没有就是配额用完。`ON CONFLICT ... DO UPDATE ... WHERE` 在行锁下对最新版本的行重新判断，所以任意多个并发调用（含两个副本）里恰好有"见到 used < 限额"的那些成功，放行次数恰好等于限额；`WHERE $4 > 0` 让当天的第一次插入也服从 0 的限额；限额降到已用量以下时一律拒绝。**读取发出前才发现什么都没发出去的，退回一次**（`Release`：`UPDATE ... SET used = used - 1 WHERE ... AND used > 0`，从不为负）：做法是先取、读完发现没发出去再退，因为"取"必须在读取器里发生的一切之前，而"发没发出去"要读完才知道。读取器在**即将连接**一个通过了地址规则的地址时通知调用的 context（`fetch.WithDialed` 与 `fetch.Dialing`，`checkedDial` 里解析与公网检查之后、拨号之前调用）。**所以：**读取器没有通知过就以错误结束的，退回：地址解析到内部（`address_not_public`）、名字解析不了、第一次连接之前就超时；通知过的，不管之后怎样都算：站点答了非 2xx、连接中断、太慢、robots.txt 不许（robots.txt 的请求已经发出）、页面太大、需要脚本，以及**第一次请求发出之后**重定向到内部地址（第一次请求是发出去的）。退回失败只记日志，这一次就多算了：只会多算，不会少算。DNS 查询不算"发往网站"，所以名字解析不了的读取不占配额；搜索没有这条例外：调用服务商之前没有别的检查可以失败。**超出：**模型读到"the daily limit of searches (page reads) for your organisation is used up; it resets at 00:00 UTC: do not try again today, and answer with what you have"；审计 `decision=refused`、`reason=quota_exceeded`，`idp_web_refused_total{tool,reason="quota_exceeded"}` 计数。**数据库不可用或慢：**每条语句有超时（缺省 2 秒，`IDP_WEB_DATABASE_TIMEOUT_MS`），超时、连接失败、没有配置数据库（启动检查之外的最后防线）、任何错误，一律**拒绝这次调用**，模型读到"the daily limit of searches (page reads) could not be checked just now, so the call was not made: try again in a little while, or answer with what you have"，审计 `decision=refused`、`reason=quota_unavailable`（日志里有原因 `the daily quota could not be checked`），不外发、不计数：宁可不可用，不可无上限（平台 ADR-0003 附录 D 第 9 节）。**没有限额的租户在配额上不碰数据库**，所以一个没设配额的部署，数据库挂了，配额不受影响；但见 D29：配置了数据库的部署，编号也在数据库里。**限额为 0 的调用不碰数据库就拒绝**，句子与超出相同。**启动检查：**租户文件里任何地方（含 `default`）写了限额而没有配置数据库，服务启动即退出（2），错误写明是哪些条目和该设什么 | 没有每日上限，按量计费的搜索服务就没有成本上限（平台 ADR-0003 D6）；内存里的计数重启清零、多副本各算各的，等于没有。"原子"是二值保证：读后写在两个副本并发时放行的次数多于限额，反向验证里实测 98 到 100 次对 50（第 9 节）。"先取后退"而不是"先看后取"：先看再取是读后写。"算什么"写成规则并各有测试，因为配额是钱：多算让租户少得，少算让上限形同虚设，所以不确定时（服务商的任何答复、读取器通知过的任何结局）一律算。`TestASearchOverTheDailyQuotaIsRefusedWithASentenceTheModelCanActOnAndTheProviderIsNotCalled`、`TestTheQuotaIsThereAgainAtMidnightUTC`、`TestASearchThatTheProviderAnswersWithAnErrorStillCounts`、`TestAReadCountsWhenItWentOutWhateverItEndedWithAndNotWhenNothingLeftTheService`、`TestTheRealReaderTellsWhichReadsSentSomething`、`TestACacheHitDoesNotCountAgainstTheDailyReads`、`TestALimitOfZeroAllowsNoneAndDoesNotAskTheDatabase`、`TestWhenTheDatabaseDoesNotAnswerACallUnderALimitIsRefusedAndNothingGoesOut`、`TestTwoReplicasConsumingOneTenantsQuotaAtTheSameTimeAllowExactlyTheQuota`（两个副本、64 个协程同时放开、每个 4 次，共 256 次抢 50 个，25 轮，每轮换一个租户）、`TestTwoReplicasOfTheServiceShareOneTenantsDailySearchesExactly`（整个服务两个副本、三天）、`TestTheCountSurvivesARestart`、`TestTheNextUTCDayStartsAtZeroAndADayIsTheUTCDayWhateverTheZone` |
| D29 | **状态放 PostgreSQL：配额与来源编号（D19）在同一个库，所以可以跑多个副本；没有数据库时仍可单副本运行，只是不能设每日限额** | **表**（`migrations/0001_init.sql`）：`daily_usage(tenant text, day date, kind text, used integer, PRIMARY KEY (tenant, day, kind))`；`source_callers(caller bytea PRIMARY KEY, next integer, last_used timestamptz)`；`source_numbers(caller bytea REFERENCES source_callers ON DELETE CASCADE, address bytea, n integer, last_used timestamptz, PRIMARY KEY (caller, address))`。**存什么：**计数、号码，和调用者与地址的 SHA-256（32 字节，`CHECK length = 32`）。**不存：**查询词、地址、页面、调用者的主体（平台 ADR-0003 附录 D 4.4"查询词和地址不进这张表"）；审计仍是日志行（D23）。`caller` 是租户与主体拼成的 `Owner()` 的**完整** SHA-256，审计行里的 `caller` 是同一个摘要的前 4 字节，所以能用前缀把一行对到审计行；表里不能只用 4 字节（32 位，一万个调用者里就有约 1% 的概率两人共用一套编号）。`TestWhatIsStoredIsTheHashesAndNotTheAddressTheCallerOrTheQuery` 以所有者的身份把三张表按文本全部读出，查找地址、主体、查询词（含它们的十六进制，因为 bytea 以十六进制显示）并核对列里是摘要。**限制：**地址的摘要没有密钥，能读库的人可以对一个猜到的地址验证它是否出现过（也能看到每个调用者用了多少地址、什么时候）；这一点由库的访问控制负责，不靠哈希。**号码的生命周期原样保留 D19：**一个调用者的编号表从第一次调用起，一小时没用就整个忘掉，再来从 1 开始；每次使用都续一小时；每个调用者最多 1000 个地址，超了丢最久没用的地址，**它的号不再给别人**（`next` 只增不减）。内存里"调用者数上限 1 万"的角色，在库里由清理担任（见下）。**分配（`Store.Number`，一个事务）：**`INSERT INTO source_callers (caller, next, last_used) VALUES ($1, 1, $2) ON CONFLICT (caller) DO NOTHING`；`SELECT next, last_used FROM source_callers WHERE caller = $1 FOR UPDATE`（**调用者的行锁：同一个调用者的分配在各副本之间排队，不同调用者互不等待**）；若 `now - last_used > 1 小时`：`DELETE FROM source_numbers WHERE caller = $1`、`next = 1`；对每个地址（按给定的顺序）：`UPDATE source_numbers SET last_used = $3 WHERE caller = $1 AND address = $2 RETURNING n`，没有行就取 `n = next; next++` 并 `INSERT INTO source_numbers (caller, address, n, last_used) VALUES (...)`；最后 `UPDATE source_callers SET next = $2, last_used = GREATEST(last_used, $3) WHERE caller = $1`，若新增了地址：`DELETE FROM source_numbers WHERE caller = $1 AND address IN (SELECT address FROM source_numbers WHERE caller = $1 ORDER BY last_used DESC, n DESC OFFSET 1000)`。一次搜索把列出的所有地址一次传入（一次往返，号码按列出的顺序连续）。时间用副本自己的时钟（测试可注入），副本之间差几秒对一小时的寿命没有意义。**清理（`Sweep`，每个副本每 10 分钟，删除是幂等的）：**空闲超过一小时的调用者（地址随之级联删除）与超过 35 天的配额日。所以表的大小只取决于一小时内活跃的调用者与近 35 天的租户，不会无限增长；清理删掉一个正在被分配的调用者的行时，分配在锁那一步发现行没了，重来（最多 4 次）。**数据库不可用时的编号：**号码拿不到，这次调用失败：`numbering_unavailable`（`decision=failed`），模型读到"the sources of this answer could not be numbered just now: try again in a little while, or answer with what you have"；不退回到内存编号，因为那会给同一个调用者两套号，让 `[n]` 指向两个来源，正是 D19 要防的。读取是读完之后才编号（D19 的"没给过的地址不占号"不变），页面已经进了缓存，所以数据库恢复后重试不会再去请求网站（`TestATenantWithNoLimitNeverAsksForTheQuotaAndNeedsTheDatabaseOnlyToNumberTheSources`）。**所以：配置了数据库的部署，数据库挂了，没有设限额的租户也做不了搜索与读取**（只是不在配额上碰库）；"没设限额的部署不受数据库影响"只对没有配置数据库的部署成立。**哪些功能要数据库：**每日限额，要；跨副本一致、重启后不变的编号，要；**不要：**读取缓存、滑动窗口限流（D21、D22，按副本，性能与滥用防护，不关乎正确性）、`ref`、审计、其余全部。**没有配置数据库（`IDP_WEB_DATABASE_URL` 与 `_FILE` 都没设）：**编号用 D19 的内存表，原样；租户文件里写了每日限额就是配置错误（退出 2，写明哪些租户、该设什么）；一个部署仍可不带数据库、单副本运行，与以前相同。**设置（与 idp-mcp、idp-host 同一套：迁移内嵌、所有者与服务两个角色、启动时核对版本）：**`IDP_WEB_DATABASE_URL`（或 `IDP_WEB_DATABASE_URL_FILE`，挂载的 Secret；两者同设是错误；本服务的密钥一律来自文件，D24，而 URL 带密码）：服务角色的 URL，**必须是 `idp_web_app` 的成员，不能是超级用户或 BYPASSRLS**，`Open` 核对并拒绝；TLS 是 URL 的：`sslmode=verify-full&sslrootcert=...`（镜像没有 CA，系统根证书由 `x509roots/fallback` 兜底，D13；私有 CA 用 `sslrootcert`）；`IDP_WEB_DATABASE_TIMEOUT_MS`（缺省 2000）。`idp-web migrate`（`IDP_WEB_MIGRATION_DATABASE_URL[_FILE]`，所有者）应用内嵌迁移，一个迁移一个事务，咨询锁 72580 串行；`idp-web ensure-login USER`（密码在 `IDP_WEB_APP_DB_PASSWORD[_FILE]`）建登录角色并加入 `idp_web_app`；`idp-web` 启动时核对库的版本，不一致就退出并写"run idp-web migrate"。角色 `idp_web_app` 只有三张表的 SELECT、INSERT、UPDATE、DELETE（DELETE 为 `Release` 与清理），不能建表、改表、清空表（`TestTheMigrationsApplyOnceAndTheServiceRoleHasOnlyTheTablePrivilegesItNeeds`）。连接有 TCP 保活与 `TCP_USER_TIMEOUT`（从 idp-mcp 复制的 `dial*.go`，一个消失的对端几秒内被发现，不是等内核 15 分钟）。**就绪：**配置了数据库时，`/readyz` 还要数据库在超时内应答，否则 503 并写明 `database not reachable`；存活探针不问数据库（数据库切换不应重启服务）。**副本：**可以多于一个，前提是配置了数据库。**仍然按副本的：**读取缓存与滑动窗口限流：N 个副本时，一个调用者在窗口内最多可做 N 倍的调用，**每日配额才是精确的上限**。别的没有阻止多副本的东西：refs 用密钥签名、任何有密钥的副本都能验证；租户文件每个副本各读各的 | 平台 ADR-0003 附录 D 选择点 2 选了 (a)：平台已有 PostgreSQL，其余组件都这样做；(b) 放内存的配额在重启后失效，等于没有上限；(c) 放网关，网关不知道哪次读取命中了缓存。编号入库是 W6 的"顺带"：配额已经要库，编号放在同一个库里，副本才能多于一个。行锁而不是一条语句：编号要"查、没有再给下一个号、更新下一个号"，用调用者的行锁把同一调用者的分配排队，比可序列化隔离加重试简单，且只有同一调用者之间等待。号码在库里与内存里同一套寿命，测试逐条对照（`TestANumberingIsForgottenAfterAnHourWithoutUseRenewedByUseAndBoundedInAddresses`）。`TestTwoReplicasGiveTheSameNumberToTheSameAddressAndDifferentNumbersToDifferentOnesUnderConcurrency`（两个副本、每个 24 个协程、三个调用者、25 个地址、各自的顺序与批量：每个地址只得一个号，号码恰好是 1 到 25 各一次）、`TestTheNumbersSurviveARestartAndAnotherReplicaSeesThem`、`TestTheNumbersAreTheSameOnAnotherReplicaAndAfterARestart`、`TestTheSweepRemovesIdleNumberingsAndOldQuotaDaysAndNothingElse`、`TestASlowDatabaseIsAnErrorWithinTheTimeoutAndAClosedOneAnswersAtOnce`（别人持有表锁，语句 300 毫秒后出错）、`TestOpenRefusesASuperuserAnUnmigratedSchemaAndAnUnreachableDatabase`、`TestReadyzReportsTheDatabaseAndTheURLMayComeFromAFile`、`TestAnyDailyLimitWithoutADatabaseIsAConfigurationError` |
| D30 | **新增依赖：`github.com/jackc/pgx/v5` v5.12.0（许可证 MIT），及它带来的 `github.com/jackc/pgpassfile` v1.0.0、`github.com/jackc/pgservicefile` v0.0.0-20240606120523-5a60cdf6a761、`github.com/jackc/puddle/v2` v2.2.3（都是 MIT）与 `golang.org/x/sync` v0.25.0（BSD-3-Clause，只用到 `semaphore`）** | idp-mcp、idp-host、idp-cmi 用的是同一个驱动同一个版本，idp-web 是第一次有数据库。不用 ORM，SQL 写在 `internal/state`，迁移内嵌（`migrations/`）。链进二进制的只有 `pgx/v5`、`pgxpool` 与上面几个；`go.mod` 里另有 `kr/text`、`rogpeppe/go-internal` 两条 `// indirect`，是 `go mod tidy` 为 pgx 的测试依赖记下的，不进二进制。`govulncheck ./...` 无漏洞。二进制仍是静态的（`CGO_ENABLED=0`），镜像仍是 `FROM scratch` |
| D31 | **PDF：有文字层的 PDF 读成带 `[page N]` 标记的文本；解析库是 `github.com/go-pdfkit/extract` v0.11.0（BSD-3-Clause），按事先写定的规则从 7 个纯 Go 的候选里选出；解析在子进程 `idp-web-pdf` 里做，自己给自己加时间、内存、系统调用的限制；工具链随之升到 Go 1.27（`go 1.27.1`，`toolchain go1.27.4`）** | 见第 10 节。范围（平台 ADR-0003 附录 D 4.3）：有文字层的 PDF，不做 OCR；扫描件、加密的、损坏的各有自己的错误（`pdf_no_text_layer`、`pdf_encrypted`、`pdf_malformed`），另有 `pdf_too_large`（压缩炸弹或内存上限）、`pdf_too_slow`、`pdf_busy`、`pdf_reader_unavailable`；下载沿用 5 MiB 与 15 秒；最多读 200 页，更长的读前 200 页并在文本开头和首行写明；输出照旧 12 KiB 一段，引用的 `sha256` 仍是交给模型那一段的摘要。**判断是不是 PDF 看字节，不只看标签**：正文以 `%PDF-` 开头就是 PDF（服务器会把它标成 octet-stream、text/plain、text/html 或不标）；标成 `application/pdf` 而内容是 HTML 页面的，按 HTML 读；标成 PDF 而什么也不是的，`pdf_malformed`。其他类型照旧不读、不下载正文。D10 与 D15 里"PDF 暂不支持"（`ErrPDF`）随之取消。测试：`internal/pdfreader`（解析、规则、对抗文件、子进程限制）、`internal/pdfread`（Runner：期限、杀进程组、输出上限、并发、取消、缺程序）、`internal/fetch/pdf_test.go`（类型判断）、`internal/tools/pdf_test.go`（首行、分段、引用、每个错误的句子与原因）、`cmd/idp-web/pdf_test.go`（启动自检、关闭） **补记（跟进）：**文件大小限制为 0 是有意的，所以输出到文件（`> out.txt`）本来只会无声地以 1 结束；现在在施加限制之前检查标准输出与标准错误是不是普通文件，是就以原因词 `unavailable`（不是关于文档的原因，评测的打分程序不认识它）、一句说明要接管道的话和退出码 2 结束；检查之后的写入遇到 EFBIG 同样如此，SIGXFSZ 被忽略（`TestOutputToAFileIsRefusedWithAMessageThatSaysToPipeIt`、`TestAWriteToAFileThatMeetsTheLimitIsReportedAsTheReaderNotTheDocument`、`TestOutputToAPipeIsReadAsBefore`）。 |
| D32 | **已检查的地址按 Happy Eyeballs（RFC 8305 第 4、5 节）拨号：交错地址族，250 ms 错开，先连上的赢，其余取消、晚到的关闭** | 原来按解析器的顺序一个接一个地拨，第一个地址是不通的 IPv6 时，整个预算（或每个死地址的超时）花在它身上；真实的例子是 www.fedex.com 在没有 IPv6 路由的网络上。检查、不重新解析、代理都没有变。默认 IPv4 先拨，可用 `IDP_WEB_DIAL_FAMILY` 改；细节与反向验证见本文件末尾的“D32”一节。 |

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

## 5. 反向验证（任务 W4）

每个控制手工去掉一次，只跑相关的包，记下哪些测试失败，再恢复。下表是这样得到的（脚本只是把这一过程自动化，改动在每次运行后还原）。

| 控制 | 去掉什么 | 失败的测试 |
|---|---|---|
| D16 `web_fetch` 校验 `ref`（T1、T2） | 不调用 `Keys.Check` | `TestAnAddressThatWasNotInASearchResultIsRefusedWithAnyRefAndWithNone`、`TestAnAlteredRefAnotherAddressesRefAnotherCallersRefAndAnExpiredRefAreRefused`、审计行测试 |
| D16 `ref` 绑定调用者 | MAC 里不含调用者 | `TestARefMadeForAnotherAddressOrAnotherCallerIsRefused`、`TestAnAlteredRef…` |
| D16 `ref` 绑定地址 | MAC 里不含地址 | `TestARefMadeForAnotherAddressOrAnotherCallerIsRefused`、`TestAnAddressThatWasNotInASearchResultIsRefused…`、`TestAnAlteredRef…` |
| D16 过期 | 不检查过期 | `TestAnExpiredRefIsRefusedAsExpired…`、`TestAnAlteredRef…` |
| D16 无歧义拼接 | 去掉长度前缀 | `TestThePartsAreJoinedWithoutAmbiguity` |
| D16 轮换 | 只用第一把密钥验证 | `TestAfterARotationTheOldKeysStillVerifyAndOnlyTheFirstSigns` |
| D16 密钥长度 | 接受短密钥 | `TestKeysShorterThan32BytesAreRefused`、`TestKeysAreReadOneALineFromAFile`、`TestAShortRefKeyAndABadTenantsFileAreRefusedByTheirContent` |
| D16 常量时间比较 | （没有去掉：时间差不是单元测试能稳定断言的；代码用 `subtle.ConstantTimeCompare`，由评审读代码确认） | 无 |
| D18 租户未启用（fetch） | web_fetch 不查 `enabled` | `TestATenantThatIsNotEnabledGetsNothing` |
| D18 租户未启用（search） | web_search 不查 `enabled` | `TestATenantThatIsNotEnabledGetsNothing` |
| D18 重定向的域名检查 | 读取时不带租户的规则 | `TestAnExcludedDomainIsRefusedOnFetchAndOnARedirectToIt` |
| D18 读取器支持调用级规则 | `WithAllow` 被读取器忽略 | `TestAnAllowRuleOfTheCallSeesEveryAddressAndCanRefuseARedirect`、`TestAnExcludedDomainIsRefused…` |
| D20 第一个地址在读取之前的检查 | 去掉预检（读取器仍会拒绝，但已占了配额） | `TestARefusedReadDoesNotUseTheReadQuota` |
| D20 结果按租户名单过滤 | 不过名单 | `TestResultsThatFailTheAddressRulesOrTheTenantsListsAreDroppedAndCounted` 等 4 个 |
| D20 结果过地址规则 | 不过 `addr.TargetWith` | 10 个端到端测试（私网与畸形地址进了结果） |
| D18 `web_open_url` 开关 | 不查 `openUrl` | `TestWebOpenURLIsRefusedUnlessTheTenantAllowsItAndThenNeedsNoRef` |
| D18 未知键 | `KnownFields(false)` | `TestAKeyTheFileDoesNotKnowIsAnErrorAndSoIsADomainThatIsNotAName` |
| D18 排除优先 | 包含优先于排除 | `TestExcludeWinsOverIncludeAndAnEmptyIncludeListMeansEveryDomain`、`TestADomainEntryCovers…` |
| D18 域名匹配 | 按文字后缀匹配（`notexample.com`） | `TestADomainEntryCoversTheNameAndItsSubdomainsButNotALookalike` |
| D18 未列出的租户 | 返回启用的策略 | `TestATenantNotNamedGetsTheDefault…`、`TestATenantThatIsNotEnabledGetsNothing` |
| D18 `openUrl` 缺省 | 缺省为开 | `TestOpenURLIsOffUnlessTheFileTurnsItOn`、`TestWebOpenURLIsRefused…` |
| D19 同一地址同一个号 | 每次都给新号 | `TestTheSameAddressKeepsItsNumberAcrossSearchesAndANewOneGetsTheNext` 等 3 个 |
| D19 按调用者编号 | 所有调用者共用一张表 | `TestTheSameAddressKeepsItsNumberAcrossSearchesAndANewOneGetsTheNext` |
| D21 缓存 | 读完不放进缓存 | `TestOffsetContinuesALongPageFromTheCacheWithoutAnotherRequestToTheOrigin`、限流测试 |
| D21 按租户隔离 | 键里不含租户 | `TestOffsetContinuesALongPage…`、`TestThePageCacheIsBounded…` |
| D21 一小时 | 不过期 | `TestOffsetContinuesALongPage…`、`TestThePageCacheIsBounded…` |
| D21 条数上限 | 只限字节 | `TestThePageCacheIsBoundedInEntriesAndBytesAndDropsTheLeastRecentlyUsed` |
| D22 搜索限流 | 不检查 | `TestTheRateLimitIsPerCallerAndTellsTheModelWhenToTryAgain` |
| D22 读取限流 | 不检查 | 同上 |
| D22 按调用者 | 所有调用者共用一个计数 | 同上 |
| D22 滑动 | 调用记录不过期 | 同上、`TestTheLimiterSlidesAndRefusesTheCallerOverTheLimitWithoutCountingTheRefusal` |
| D23 审计里没有 `ref` | 把 `ref` 写进 `url` 字段 | `TestTheAuditLineHasTheQueryAndTheAddressAndNeitherTheRefNorAKeyNorPageText` |
| D23 审计里有查询词 | 不写查询词 | 同上 |
| D23 审计里没有主体 | 写主体而非短哈希 | 同上 |
| D17 请求里没有标识（Exa） | 请求头加 `X-Request-Id` | `TestTheExaRequestCarriesTheDocumentedFieldsAndNothingElse` |
| D17 请求里没有标识（Brave） | `User-Agent` 写进租户 | `TestTheBraveRequestCarriesTheDocumentedFieldsAndNothingElse` |
| D17 查询词里没有标识 | 查询词后面拼上租户号 | `TestWhatTheProviderIsSentHasTheQueryAndTheTenantsDomainListsButNotWhoAsked` |
| D17 服务商错误不外泄 | 401 时把应答正文放进错误 | `TestAProviderErrorGivesAFixedSentenceAndTheDetailGoesToTheLog` |
| D17 标题压成一行 | 不压 | `TestATitleOrExcerptCannotWriteALineOfTheResultList` 等 3 个 |
| D17 应答大小 | 去掉 4 MiB 限制 | `TestAnAnswerLargerThanTheLimitIsRefusedEvenWhenItsJSONIsComplete` |
| D17 不跟随到另一个主机 | 不设 `CheckRedirect` | `TestAProviderRedirectToAnotherHostIsNotFollowed` |
| D17 代理 | 不用环境里的代理 | `TestTheProviderClientUsesTheProxyOfTheEnvironment` |
| D17 超时 | 去掉客户端超时 | `TestTheProviderClientHasItsOwnTimeout` |
| D24 每个哨兵有句子 | 去掉 `ErrExpired` 或 `fetch.ErrRobots` 的一行 | `TestEverySentinelHasAFixedSentence`（前者另有 `TestAnAlteredRef…`） |
| D24 拒绝按原因计数 | 不调用 `Refused` | `TestAnAlteredRef…`、`TestTheRateLimitIs…`、`TestWebOpenURLIsRefused…` |
| D24 必须设服务商 | 缺省为 fixture | `TestEachMissingOrInvalidSettingIsAConfigurationError` |
| D24 `/readyz` | 总是就绪 | `TestReadyzIsNotReadyUntilTheProviderTheRefKeyAndTheTenantsAreLoaded` |
| D24 Exa 基址必须是 https | 接受 http | `TestEachMissingOrInvalidSettingIsAConfigurationError` |

没有单独失败的一处是有意的：D16 的常量时间比较（见上）。

## 6. 其余

搜索、`ref`、租户配置、编号、缓存、限流、审计见 D16 到 D25；地址检查、读取、提取见 D5 到 D15。已知差距：每次运行的调用上限（D22）；`ref` 的常量时间比较没有测试。编号的跨副本一致（D19）已由 D29 补上（需要数据库）；滑动窗口仍是按副本的（D22）。每日配额与数据库见 D28 到 D30。

## 7. 反向验证（任务 P2）

| 决策 | 破坏 | 哪个检查失败 |
|---|---|---|
| D26 出口策略与代码一致 | 从 `web-egress` 的 `except` 里删掉 `100.64.0.0/10` | `TestEgressPolicyIsTheOnlyWayOutAndIsIPv4Only`（`100.64.0.0: the service refuses it: true; the policy excludes it: false`） |
| D14、D26 清单里没有测试例外 | 往 `web.yaml` 的环境变量里加 `IDP_WEB_TEST_ORIGINS` | `TestNothingTestOnlyIsShipped` |
| D26 发布的出口规则排除集群内地址 | 在平台的 kind 验收里去掉测试专用的 NetworkPolicy `idp-web-test-origins`（`tests/kind/web-origin.yaml`，只在平台仓库的测试文件里） | PATH M 失败：模型读到 `the site did not deliver the page within 15 seconds`（连接被集群的策略丢弃，等不到响应；这句话对应审计原因 `too_slow`）；放回去 PATH M 与 N 通过 |

## 8. 反向验证（D27，平台 ADR-0003 附录 D 第二阶段的任务 W4）

| 决策 | 破坏 | 哪个检查失败 |
|---|---|---|
| D27 需要脚本的页面不当作页面 | `notThePage` 里去掉对 `needsScript` 的调用 | `TestAnEmptyMountPointWithLittleTextIsAPageThatNeedsScript`、`TestANoscriptNoticeAskingForJavaScriptWithLittleTextIsAPageThatNeedsScript`、`TestAnArticleThatOnlyExistsInAScriptStringIsAPageThatNeedsScript`、`TestTheFallbackNeitherMasksTheDetectionNorIsOverruledByIt`、`TestTheReaderAnswersAPageThatNeedsScriptWithItsErrorAndReadsAShortOne` |
| D27 `script_prose` | `needsScript` 的 `switch` 里停用这一条 | `TestAnArticleThatOnlyExistsInAScriptStringIsAPageThatNeedsScript`（四个形状各一行：`a CMS variable, Chinese: rule "" error <nil>, want "script_prose"`） |
| D27 `placeholder_mount` | `needsScript` 的 `switch` 里停用这一条 | `TestAMountPointThatOnlyHoldsALoadingLineIsAPageThatNeedsScript`（`loading: rule "" error <nil>, want "placeholder_mount"`，另四个写法同） |
| D27 `skeleton` | 停用这一条 | `TestPlaceholderBlocksInPlaceOfTheContentMakeAPageThatNeedsScript`（三个框架各一行） |

## 9. 反向验证（D28 到 D30，平台 ADR-0003 附录 D 第二阶段的任务 W6）

每一行都是把控制去掉一次、跑对应的测试、看它失败、再放回去（`git diff` 与备份比对确认已还原）。数据库测试在一次性的 PostgreSQL 17 上跑（`make test-integration`），其余是 `go test -race`。

| 决策 | 破坏 | 哪个检查失败 |
|---|---|---|
| D28 原子扣减 | `Take` 改成先 `SELECT` 已用量、小于限额再 `INSERT ... ON CONFLICT DO UPDATE SET used = used + 1`（读后写） | `TestTwoReplicasConsumingOneTenantsQuotaAtTheSameTimeAllowExactlyTheQuota`，**第 0 轮**就失败，三次运行三次：放行 98、100、100 次，配额是 50（"round 0: 100 allowed, 156 refused"）。测试是够强的：256 次尝试抢 50 个，没有一次放对 |
| D28 键里有日期 | `day()` 返回一个固定日期（所有日子共用一行） | `TestTheNextUTCDayStartsAtZeroAndADayIsTheUTCDayWhateverTheZone`（"the next day did not start at zero with its own two"）、`TestTheSweepRemovesIdleNumberingsAndOldQuotaDaysAndNothingElse` |
| D28 数据库不可用时拒绝 | `take` 在数据库出错时返回 `nil` 而不是 `quotaUnavailable` | `TestWhenTheDatabaseDoesNotAnswerACallUnderALimitIsRefusedAndNothingGoesOut`、`TestWhenTheDatabaseIsGoneACallUnderALimitIsRefusedAndNothingGoesOut`（后者用真实的数据库，连接池已关闭） |
| D28 没有数据库时拒绝 | `State == nil` 时放行 | `TestWithNoDatabaseATenantWithALimitIsRefusedAndOthersAreNumberedInMemory` |
| D28 启动检查 | 去掉"限额而无数据库"的错误 | `TestAnyDailyLimitWithoutADatabaseIsAConfigurationError` |
| D28 没有限额的租户不碰数据库 | 无限额时也调用 `Take` | `TestATenantWithNoLimitNeverAsksForTheQuotaAndNeedsTheDatabaseOnlyToNumberTheSources` |
| D28 0 表示一次都不允许 | 去掉工具层对 `limit <= 0` 的拒绝 | `TestALimitOfZeroAllowsNoneAndDoesNotAskTheDatabase` |
| D28 0 表示一次都不允许（SQL） | 去掉首次插入的 `WHERE $4 > 0` | `TestALimitOfZeroAllowsNothingEvenOnTheFirstCallOfTheDayAndALowerLimitRefusesWhatWasUsed` |
| D28 配额在滑动窗口之后 | 把"取配额"移到滑动窗口之前 | `TestACallThatIsRefusedBeforeItGoesOutDoesNotTouchTheQuota`（"a search refused by the sliding window took a unit of the daily quota"） |
| D28 搜索不检查配额 | 去掉 `web_search` 里的 `take` | `TestASearchOverTheDailyQuotaIsRefusedWithASentenceTheModelCanActOnAndTheProviderIsNotCalled` |
| D28 服务商出错的搜索也算 | 服务商出错时退回一次 | `TestASearchThatTheProviderAnswersWithAnErrorStillCounts`（四个子测试：busy、unavailable、timeout、unknown） |
| D28 缓存命中不算 | 在查缓存之前就取配额 | `TestACacheHitDoesNotCountAgainstTheDailyReads`（"read 0 from the cache was refused"） |
| D28 没发出去的读取退回 | 去掉 `release()` | `TestAReadCountsWhenItWentOutWhateverItEndedWithAndNotWhenNothingLeftTheService`（地址解析到内部、名字解析不了、第一次连接前超时三个子测试） |
| D28 发出去了的读取不退回 | 读取失败一律退回 | 同一个测试（站点答错误状态、连接中断、太慢、robots、太大、需要脚本、第一次请求之后重定向到内部：七个子测试）、`TestTheRealReaderTellsWhichReadsSentSomething`、`TestReadsThatWentOutButFailedAreCountedInTheDatabaseAndTheLimitHolds` |
| D28 读取器通知"即将连接" | `checkedDial` 里去掉 `Dialing(ctx)` | `TestTheRealReaderTellsWhichReadsSentSomething`（真实读取器、真实源站：一个不存在的页面、一次重定向都被退回，多算变成少算） |
| D28 退回不为负 | 去掉 `AND used > 0` | `TestAUnitGivenBackIsAvailableAgainAndTheCountNeverGoesBelowZero` |
| D28 租户文件的限额 | 接受负数 | `TestADailyLimitThatIsNegativeTooLargeOrNotAWholeNumberIsAnErrorThatNamesIt` |
| D29 调用者的行锁 | `SELECT ... FOR UPDATE` 去掉 `FOR UPDATE` | `TestTwoReplicasGiveTheSameNumberToTheSameAddressAndDifferentNumbersToDifferentOnesUnderConcurrency`（并发分配撞车） |
| D29 库里存的是摘要不是地址 | `Hash(a)` 改成 `[]byte(a)`（同时去掉列上的 `CHECK length = 32`，否则是约束先拒绝，那也是一道控制） | `TestWhatIsStoredIsTheHashesAndNotTheAddressTheCallerOrTheQuery`（"the rows hold … and 68747470…, not the SHA-256 of the caller and of the address"；只去掉代码里的摘要而保留约束时，插入被 `source_numbers_address_check` 拒绝，同一个测试失败） |
| D29 一小时没用就忘掉 | 去掉过期判断 | `TestANumberingIsForgottenAfterAnHourWithoutUseRenewedByUseAndBoundedInAddresses` |
| D29 每个调用者的地址上限 | 去掉超出时的删除 | 同上 |
| D29 编号跨重启不变 | 每次分配都从 1 开始 | `TestTheNumbersSurviveARestartAndAnotherReplicaSeesThem`、同上 |
| D29 清理 | 不删空闲调用者；不删过期的配额日 | `TestTheSweepRemovesIdleNumberingsAndOldQuotaDaysAndNothingElse`（各一次） |
| D29 最小权限的角色 | `Open` 接受超级用户 | `TestOpenRefusesASuperuserAnUnmigratedSchemaAndAnUnreachableDatabase` |
| D29 就绪报告数据库 | `/readyz` 不问数据库 | `TestReadyzReportsTheDatabaseAndTheURLMayComeFromAFile` |

**没有做的反向验证：**`Release` 失败时"多算不少算"（只在日志里留一行，没有单独的测试）；TLS 选项（URL 的 `sslmode` 由 pgx 解析，本仓库没有 TLS 的数据库可测，`verify-full` 没有实测）；连接保活与 `TCP_USER_TIMEOUT` 只有从 idp-mcp 复制来的套接字选项测试（`TestGuardDial`），没有拔网线的实测。

## 10. D31 的细节（任务 W5，平台 ADR-0003 附录 D 4.3）

**10.1 规则（在量之前写定，量过之后没有改）。**

1. 判据，在开发集（E4-PDF 的 20 份，留出集 10 份不碰）上：计分段落保留 ≥ 95%（按语言的数字只报告、不作判据）；三份扫描件报 `no_text_layer`；每份 ≤ 5 秒、≤ 256 MiB；对抗文件 15 个全过（在时限内以原因词结束，不输出文字，不超内存）。
2. 候选：纯 Go（不要 cgo、不要外部程序）、许可证允许随闭源部署发布（MIT、BSD、Apache-2.0；AGPL/GPL 不要；商业许可的库不要）、有维护（证据：最近一次提交与版本的日期、是否有测试与模糊测试）、能提取 CID/Identity-H 字体与 ToUnicode 映射的文字（E4-PDF 的中文文档全是这样的）。库需要修补才能过的，记为发现，不悄悄绕过；包一层排序或限额的小封装可以，分叉由评审决定。
3. 选择：达标的候选里，依赖少而小的；仍相同则段落保留率高的；记录落选者的数字。没有一个纯 Go 候选达标，就不降判据、不对着开发集调参：停下来报告。
4. 留出集不属于实现者：不打开留出集的文档与标注，评审只跑一次。

**10.2 候选（2026-11-19，Go 模块代理与 pkg.go.dev 上找到的；"评估"一栏写是否量过）。**

| 库 | 版本 | 许可证 | 新增依赖 | 维护 | 结论 |
|---|---|---|---|---|---|
| `github.com/go-pdfkit/extract` + `reader` + `pdffont` | v0.11.0 + v0.10.0 + v0.4.0（extract 的 go.mod 声明的组合） | BSD-3-Clause | `go-opentype/opentype` v0.17.0、`dsnet/compress`（间接）；`reader` 本身零依赖；**要求 Go 1.27.1** | 2026-11-09 发布；有模糊测试、覆盖率声明；2026 年新库，使用面还小 | **候选，评估，达标，当选** |
| `github.com/ledongthuc/pdf` | v0.0.0-20261020125853-5e46805f718e | BSD-3-Clause | 无 | 2026-10-20 有提交；rsc/pdf 的分叉 | 候选，评估，**不达标**：79.2% |
| `github.com/tsawler/tabula` | v1.6.14 | MIT | `golang.org/x/image`、`x/net`、`x/text`（OCR 的 `gosseract` 只在 `-tags ocr` 下编进去） | 2026-06-29 | 候选，评估，**不达标**：58.3%，另有一份超时、一份超内存 |
| `github.com/mattermost/pdf`、`hanzoai/pdf` v1.2.1、`krystophny/pdf`、`digitorus/pdf` v0.3.0、`Geek0x0/pdf` v1.3.1 | 各自最新 | BSD-3-Clause | 无（Geek0x0 要 `x/sys`） | 2026-02 到 2026-11 | 都是 rsc/pdf（与 ledongthuc 同源）的分叉，评估，**都不达标**：79.2%、79.2%、62.5%、43.1%、0%（Geek0x0 打不开带交叉引用流的文件） |
| `github.com/go-pdfkit/extract` 的上一层 `go-pdfkit/reader` v0.19.1（最新） | v0.19.1 | BSD-3-Clause | 无 | 2026-11-18 | 同一族；用 extract 声明的 v0.10.0 与最新的 v0.19.1 各量过一次，数字相同，按 extract 自己声明的组合 |
| `github.com/pdfcpu/pdfcpu` | v0.16.4 | Apache-2.0 | 多（cobra、yaml、x/image 等） | 活跃 | 不是候选：它的 API 取内容流、图片、字体，没有文字提取 |
| `github.com/benoitkugler/pdf` | v0.0.16 | MIT | `x/image`、`x/text`、`hhrutter/lzw` | 2026-11-19 | 不是候选：读写 PDF 的对象模型，没有页面文字提取 |
| `github.com/unidoc/unipdf/v4` | v4.11.0 | 商业许可（需要付费密钥） | | 活跃 | 排除：许可证不能随闭源部署发布，且按密钥计量 |
| `github.com/klippa-app/go-pdfium` | v1.21.5 | MIT | wazero、go-plugin、hclog 等 10 个以上，PDFium 编成 WebAssembly 的二进制十几 MiB | 活跃 | 未评估：用的是 PDFium 的 C++ 代码（WebAssembly 里），不算"纯 Go 的解析"，依赖和体积都远大于入选者；按规则 3 它只在入选者不达标时才有意义 |
| `github.com/gen2brain/go-fitz`、`richardwilkes/pdf`、`Servicewall/zpdf-go` | v1.28.3、v1.27.3、v0.3.5 | AGPL（MuPDF）/ MPL-2.0 包 MuPDF（AGPL）/ MIT 包 Zig 写的 zpdf | cgo，带各平台的静态库 | 活跃 | 排除：cgo 与预编译的原生库，MuPDF 是 AGPL；`FROM scratch` 的镜像不带 C 运行库 |
| `heussd/pdftotext-go`、`taigrr/document-crack` | | | 调用 poppler 的 `pdftotext`；document-crack 是 ledongthuc 加 pdfcpu 的外壳 | | 排除：外部程序（计划写明不用）；外壳没有新的解析器 |
| `rsc/pdf`、`lpar/pdf`、`dcu/pdf`、`jung-kurt/gofpdf` | | BSD-3 | | 2018 年以来无更新；gofpdf 只生成 | 排除：无人维护 |

**10.3 数字（开发集 20 份：17 份有文字层、3 份扫描件；每份限 5 秒、256 MiB）。** 参照：pypdf 70/72，PDFium 72/72（参照偏高，因为计分段落是"至少一个参照找到"的）。

| 候选 | 段落保留（72 段） | 中 / 英 | 每段都保留的文档 | 扫描件报 `no_text_layer` | 超时 / 超内存 | 最慢 / 最大峰值 | 对抗 15 个 |
|---|---|---|---|---|---|---|---|
| **go-pdfkit/extract**（裸封装：每页 `extract.Runs` 加 `Assemble`，无限额） | **71/72 = 98.6%** | 34/34 / 37/38 | 16/17 | 3/3 | 0 / 0 | 0.15 秒 / 24 MiB | 14/15（炸弹：解压到 258 MiB 后被评分器杀掉） |
| **go-pdfkit/extract，出厂的 `idp-web-pdf`**（带第 10.7 节的限额） | **71/72 = 98.6%** | 34/34 / 37/38 | 16/17 | 3/3 | 0 / 0 | 0.18 秒 / 23 MiB | **15/15** |
| ledongthuc/pdf，`GetPlainText`（库自己的） | 57/72 = 79.2% | 31/34 / 26/38 | 11/17 | 1/3 | 0 / 0 | 0.18 秒 / 24 MiB | 14/15（页树成环：挂死） |
| ledongthuc/pdf，自己按坐标排行的封装 | 46/72 = 63.9% | 16/34 / 30/38 | 7/17 | 1/3 | 0 / 0 | 1.33 秒 / 24 MiB | 同上 |
| tabula，缺省 `Text()` | 42/72 = 58.3% | 21/34 / 21/38 | 3/17 | 2/3 | 1 / 1 | 5.02 秒 / 266 MiB | 13/15（炸弹、页树成环：超内存） |
| tabula，`PreserveLayout()` | 47/72 = 65.3% | | | 2/3 | 1 / 1 | 5.01 秒 / 258 MiB | 13/15 |
| mattermost/pdf 分叉，`GetPlainText` | 57/72 = 79.2% | | | 1/3 | 0 / 0 | | 15/15 |
| hanzoai/pdf 分叉，`GetPlainText` | 57/72 = 79.2% | | | 1/3 | 0 / 0 | | 14/15（页树成环：挂死） |
| krystophny/pdf 分叉 | 45/72 = 62.5% | | | 1/3 | 0 / 0 | | 14/15（同上） |
| digitorus/pdf 分叉 | 31/72 = 43.1% | | | 3/3 | 0 / 0 | | 14/15（同上） |
| Geek0x0/pdf 分叉 | 0/72（打不开带交叉引用流的文件） | | | 2/3 | 0 / 0 | | 14/15（同上） |

**10.4 选择（按规则）。** 只有 go-pdfkit/extract 过了规则 1 的段落保留判据（98.6% ≥ 95%）、扫描件、时间与内存；对抗文件它**配上限额的出厂程序**全过（裸封装 14/15，炸弹文件它自己解压到 1 GiB 的上限才停，那个上限不能从外部设置，所以第 10.7 节的预检是封装该做的事，不是对库的修补）。其余候选都在规则 1 的第一条上就落选（最好的 79.2%），不进入规则 3 的比较。所以规则 3 的"依赖少而小"没有用上：只有一个候选达标。**没有改库，没有分叉**。落选者的数字在上表；它们同源（rsc/pdf），共同的毛病是文字按内容流的顺序输出、不看坐标：行的拼接不对（英文丢空格）、表格与多栏乱序，tabula 的版面分析在这批文件上更差并且吃内存。
- **仍然失败的开发集段落**：计分的 72 段里 1 段：`hlag-9m` 第 45 页的一行日程（"Publication of half-year financial report H1 2026"）。词都读对了，输出里是同一句话，但首字母是小写的 "publication of half-year financial report H1 2026"：这份文档的这部分用小型大写字母的字体，输出里一些字母的大小写与页面不同（公司名读成 "HApAG-LLOyD"）；评分器的匹配大小写敏感，所以不过。原因没有深究（字体的编码，不是读序或表格）；不是库的崩溃或丢字。不计分的 4 段（两个参照提取器都没找到的）读到 1 段：`scan-zh-tariff`（带 OCR 文字层的 1938 年税则，竖排、横向、表格）的其余 3 个表格行没有读到，原因是竖排表格的阅读顺序。

**10.5 新增依赖（形式同 D25）。** 编进 `idp-web-pdf`（不编进 `idp-web`）的第三方模块，5 个：

| 模块 | 版本 | 许可证 | 说明 |
|---|---|---|---|
| `github.com/go-pdfkit/extract` | v0.11.0 | BSD-3-Clause | 页面文字提取：字体、ToUnicode、行与词的还原（约 1500 行） |
| `github.com/go-pdfkit/reader` | v0.10.0 | BSD-3-Clause | PDF 的解析：交叉引用、对象流、过滤器、加密（读，空密码）；零依赖（约 5700 行） |
| `github.com/go-pdfkit/pdffont` | v0.4.0 | BSD-3-Clause | 字体与编码（约 1200 行） |
| `github.com/go-opentype/opentype` | v0.17.0 | 见该模块的 LICENSE（BSD 类） | 读嵌入字体里的字形名（约 1.7 万行） |
| `github.com/dsnet/compress` | v0.0.2-20230904 | BSD-3-Clause | 间接依赖（bzip2 等） |

`go-opentype/fonts`（68 MiB 的字体数据）在 go.mod 的图里，但不被 `idp-web-pdf` 编进去（`go list -deps` 为证）。`idp-web` 自己的模块数不变（15 个）：服务进程不含 PDF 解析器（`internal/pdfread` 只有 Runner，解析在 `internal/pdfreader`，只有 `cmd/idp-web-pdf` 引用它）。**工具链**：`extract` 与 `reader` 的 go.mod 要求 `go 1.27.1`，所以本仓库的 `go.mod` 由 `go 1.26.0` 升到 `go 1.27.1`、`toolchain go1.26.11` 改为 `toolchain go1.27.4`（1.27 系列的最新补丁；1.27.1 的标准库有 14 个已知漏洞，修在 1.27.2 到 1.27.4，所以不能停在库要求的最低版本），CI 的 `setup-go` 从 1.26 改为 1.27。这是一个评审要看的决定：库新，工具链跟着升；备选是不升工具链、选不达标的库，或按计划回到带 poppler 的单独容器。

**10.6 输出、"没有文字层"、类型判断。**
- **形式**：每页以空行加一行 `[page N]` 开头，页的文字在下面；没有文字的页写 `(no text on this page)`；页里有读不出的字（字体没有映射）写 `(part of the text of this page could not be decoded)`。长度由 `extract.Page` 照旧切成 12 KiB 一段，所以页标记可能落在任何一段里；模型引用时说"第 N 页"，`citations.sha256` 仍是交给它的那一段的摘要。首行 `[n] 标题 (PDF, 30 pages; bytes 0–12288 of 80211; the rest: …)`，被截断的写 `PDF, 560 pages, only the first 200 are read`；文本开头也有一行 `[PDF, 560 pages; only the first 200 are read]`（缓存、翻页时仍看得到）。标题取文档信息里的 `/Title`，没有就用文件名。
- **表格**：库的 `extract.Text` 把同一基线上的文字拼成一行、单元格之间留空格（开发集里计分的表格段落都在 71/72 之内，唯一没过的是上面那一行日程），**不是 Markdown 表格**，没有用库的 `Tables`（版面分析的结果在这批文件上没有量过）。竖排与多栏的表格顺序可能不对。
- **没有文字层的规则**：一页里不含空白的字符 ≥ 20 个才算"有文字"；**读到的页里至少十分之一有文字**，文档才算有文字层，否则 `no_text_layer`。一个盖了馆藏章（十来个字符）的 39 页扫描件、一页上写着几个字的扫描件都是"没有文字层"；一份大半是图、其中有真正的文字页的文档读出来，没有文字的页写明。门槛是测试里的：`TestTheRuleForATextLayerIsTwentyCharactersOnOneTenthOfThePages`（1/10 过、1/11 不过；19 个字符不过、20 个过）、`TestAScanWithAStampOnOnePageHasNoTextLayer`。门槛没有用开发集调过（先定 20 与十分之一，量出来三份扫描件都对）。
- **类型判断**：见 D31 一行。`internal/fetch/body.go` 的 `page`：先看类型能不能是 PDF 或页面（HTML、XHTML、纯文本、`application/pdf`、`application/x-pdf`、octet-stream、没有类型）；读正文（上限 5 MiB，也限制 gzip 解开后的大小，与 D10 一样）；`%PDF-` 开头就交给 PDF 读取器，不管标签；标成 PDF 的但是 HTML 页面的（错误页、登录页最常见），当 HTML 读；标成 PDF 而什么也不是的是 `pdf_malformed`；octet-stream 与没有类型而不是 PDF 的，照旧 `content type … is not read`。其他类型（zip、json、图片）不读正文，即使正文以 `%PDF-` 开头。

**10.7 子进程隔离与它的限制。** 解析在 `cmd/idp-web-pdf` 里（新的小命令，没有放进 `idp-web-extract`：那个是评测用的 HTML 提取，形态相似，但 PDF 读取要进镜像、有自己的限制与协议；也没有用"同一个二进制带参数重启"：那样子进程会带着 HTTP、数据库、HTML 提取的全部代码，而 `idp-web-pdf` 只有解析器；镜像里是两个文件，`/idp-web` 与 `/idp-web-pdf`）。**评测打分的就是这个命令**（`score --cmd <路径>/idp-web-pdf`），协议：PDF 从标准输入，文本到标准输出，失败时标准错误第一行以原因词开头。服务每份 PDF 启动一个（`internal/pdfread` 的 `Runner`），启动时用内置的小 PDF 自检一次。

| 限制 | 谁施加、怎么施加 | 约束什么 | 去掉它，哪个检查失败（第 11 节） |
|---|---|---|---|
| 内存 192 MiB | 子进程在读第一个字节之前对自己 `setrlimit(RLIMIT_DATA)`（私有可写的匿名映射都算，Go 的堆是 mmap，所以 `RLIMIT_DATA` 对它有效）；`RLIMIT_AS` 只设成"内存加 4 GiB"（Go 运行时预留大量从不使用的地址空间，把 `RLIMIT_AS` 设到 1 GiB 以下连启动都不行，实测要 2 GiB 以上）；另设 `debug.SetMemoryLimit`（软的，只让 GC 更勤快，不是控制）。`GOMEMLIMIT` 是软的，所以没有靠它 | 超出的分配失败，Go 运行时中止进程，只丢这一次调用 | 是 |
| 压缩炸弹 | **预检**：先逐个流（Flate、非图片）解压到只计数的写入器里，单个流 > 48 MiB 或全部之和 > 160 MiB 就在解析之前拒绝（`too_large`）；库自己的 1 GiB 上限不可设置，所以预检是封装的责任。不探测的：滤镜链（ASCII85 加 Flate）与由间接对象指定滤镜的流，它们靠内存上限兜底 | 炸弹文件在 0.03 秒内被拒绝，不膨胀 | 是 |
| 时间 | 子进程自己的计时器（缺省 4 秒，服务传 6 秒）到点输出 `too_slow` 退出；`RLIMIT_CPU`（期限加 2 秒）是内核给的第二道；Runner 在期限加 1 秒时杀整个进程组（`Setpgid`，杀 `-pgid`）。期限取 6 秒与这次调用剩下的时间（15 秒减去下载用掉的，再留 0.5 秒）的较小者，剩下不到 0.5 秒就不启动 | 在调用的 15 秒之内 | 是 |
| 输出 | 子进程按页停（4 MiB 文本，超出的页不要，并写明）；Runner 读标准输出最多 4 MiB 加 64 KiB，超过就杀 | 输出大小 | 是 |
| 页数 | 子进程最多读 200 页，文本开头写明 | 200 页 | 是 |
| 没有网络、不能起别的程序 | 子进程用 seccomp-bpf（`PR_SET_NO_NEW_PRIVS` 加 `seccomp(2)` 的 `TSYNC`，架构不对一律 `EPERM`）让 `socket`、`connect`、`bind`、`listen`、`accept4`、`execve`、`execveat`、`ptrace` 返回 `EPERM`；其他调用放行（Go 运行时要很多，更长的名单要为每个架构与版本各写一张表）。容器里没有特权也能做（`no_new_privs` 是 `allowPrivilegeEscalation: false` 已有的），cgroup 做不了 | 解析器的漏洞不能发起连接、不能起程序 | 是 |
| 没有环境变量、没有继承的文件描述符、不写文件 | `cmd.Env` 为空；工作目录 `/`；子进程第一件事用 `close_range(3, ~0)` 关掉所有 2 以上的描述符；`RLIMIT_NOFILE` 16、`RLIMIT_FSIZE` 0、`RLIMIT_CORE` 0 | 服务的密钥、套接字、数据库连接不会漏进去 | 是 |
| 同时 2 个（`IDP_WEB_PDF_CONCURRENCY`） | `Runner` 的槽；满了，调用最多等 2 秒（或调用被取消），然后 `pdf_busy` | 内存：Pod 的上限要容下 N × 192 MiB（清单改成 1 GiB） | 是 |
| 找不到或不能用 | 启动时 `FindCommand`（`IDP_WEB_PDF_READER`，缺省与 `idp-web` 同目录的 `idp-web-pdf`）与内置 PDF 自检；任何一步失败，服务照常启动，日志写明原因，PDF 一律回答 `pdf_reader_unavailable`；`IDP_WEB_PDF=off` 同样 | 不会"每次调用都含糊地失败" | 是 |
| 限制施加不了 | 非 `linux/amd64`、`linux/arm64` 的构建里 `applyProcessLimits` 返回错误，命令拒绝运行（`unavailable`），自检失败 | 不会无限制地解析 | `TestTheCommandRefusesToRunWithoutItsLimits` |

没有做的、要知道的：内核太旧没有 `close_range`（5.9 以前）时退化为逐个关 3 到 255；seccomp 的名单只有这 8 个调用，一个能做内存损坏的解析器漏洞仍能读写这个进程自己的内存、用 CPU 到期限，但出不了网、起不了程序、写不了文件；子进程与服务共用 Pod 的 CPU；没有在真实的 Kubernetes 上跑过（容器里只用 `docker run --read-only --cap-drop ALL --security-opt no-new-privileges` 跑过自检与服务启动）。

**10.8 不处理的。** OCR（没有文字层就是没有）；需要用户密码的 PDF（`pdf_encrypted`；只有所有者密码、用户密码为空的能读）；表单字段、批注与附件里的文字；从右到左与竖排文字的阅读顺序（库按基线排行，竖排的税则表就是这样丢的）；字体没有映射的字（库把它标成不可读，我们写明"部分文字未能解码"，不猜）；多栏版面的栏序（按基线拼行，两栏会被拼成一行）；图里的字；PDF 内嵌的 JavaScript 等不执行（也不解析）。

**10.9 模糊测试与漏洞扫描。** `FuzzRead`（`internal/pdfreader/fuzz_test.go`）以 `read`（不带恢复的解析入口）为目标，种子是 `internal/pdftest` 构造的 15 份文件（文本页、压缩流、CID 字体、表格、标题、无文字页、盖章的扫描件、加密、多页、炸弹、深层页树、成环页树、HTML、只有头、空），不变式：返回文本或四种错误之一，文本是合法 UTF-8，页数一致。**运行**：先 90 秒（约 1000 万次），再 10 分钟（9250 万次，新增有趣输入 510 个，共 628 个；这两次在目录整理之前，代码相同），整理成现在的包之后在 `internal/pdfreader` 又跑 3 分钟（2400 万次，新增 340 个），**共约 13 分钟，没有发现崩溃、挂死或违反不变式的输入**；因此没有崩溃者可加成回归测试（遇到时放进 `testdata/fuzz/FuzzRead`，随普通测试运行）。模糊测试在同一进程里跑，所以发现不了"内存耗尽"类的问题，那类由子进程的限制与预检兜底，并有对抗文件和炸弹测试。**漏洞扫描**：`govulncheck` 的源码模式（`./...`）在本机报"requires go1.26 or later / generic method requires go1.27 or later"，是它的类型检查器还读不懂 Go 1.27 的标准库，不是扫描结果；改用二进制模式：`govulncheck -mode=binary` 扫 `idp-web` 与 `idp-web-pdf`（`-trimpath`、go1.27.4 构建），两个都是"No vulnerabilities found"；用 go1.27.1 构建的同一份代码扫出标准库的 14 个漏洞（net/http、crypto/x509、html/template 等，修在 1.27.2 到 1.27.4），这就是 `toolchain` 钉在 go1.27.4 的原因。CI 的 `govulncheck ./...`（`@latest`）能不能读 1.27 的标准库，要等 CI 跑一次才知道。

## 11. 反向验证（D31，平台 ADR-0003 附录 D 第二阶段的任务 W5）

每一行都是把控制去掉一次、跑对应的测试、看它失败、再放回去（脚本逐项做，结束后工作树与备份比对；`gofmt`、`go vet` 干净）。子进程的测试在**不带竞态检测、静态（`CGO_ENABLED=0`）**的测试二进制里起进程，与出厂的 `idp-web-pdf` 同一种构建：竞态检测器预留的地址空间任何地址空间上限都不允许，cgo 的线程栈会算进内存上限。

| 决策 | 破坏 | 哪个检查失败 |
|---|---|---|
| 预检：压缩炸弹在解压前拒绝 | `read` 里跳过 `preflight` | `TestAFlateBombIsRefusedBeforeItIsInflated`（"error the PDF has no text layer…, want the PDF holds more data than can be read"：炸弹被读到了头）、`TestTheCommandSpeaksTheProtocolOfTheEvaluation`（炸弹：运行时 `out of memory` 而不是 `too_large`） |
| 内存上限（`RLIMIT_DATA`） | 去掉这一项 | `TestTheMemoryLimitStopsAProcessThatAllocatesWithoutEnd`（"held 1040 MiB … peak 1047 MiB with a limit of 160"，测试里的恶意读取器在 1 GiB 自己停下，所以去掉限制不会吃掉机器）、`TestTheMemoryLimitStopsTheBombThatThePreflightWouldHaveRefused` |
| 子进程自己的期限 | 计时器改成一小时 | `TestTheCommandEndsItselfAtItsDeadline`（"took 1.99s, exit signal: killed"：这时只剩 `RLIMIT_CPU` 的第二道，2 秒后内核杀掉；预期 0.4 秒就带着 `too_slow` 退出） |
| Runner 在期限加 1 秒杀进程 | 外面的期限改成一小时 | `TestACommandThatIgnoresItsDeadlineIsKilledWithItsProcessGroup`、`TestTheDeadlineOfACallIsTheRemainingTimeOfTheCall`（测试超时：不理会期限的子进程睡一小时，没有外面的杀就没人叫醒它） |
| 杀整个进程组 | 只杀子进程（`cmd.Process.Kill()`） | `TestACommandThatIgnoresItsDeadlineIsKilledWithItsProcessGroup`（"the process the command started (…) is still running after the kill"） |
| 输出上限 | `capped.Write` 不再检查 | `TestAProcessThatWritesWithoutEndIsKilledAtTheOutputBound`（不是 `ErrTooLarge`，在期限处才被杀） |
| 页数 200 | 读全部页 | `TestADocumentOfMoreThanTheLimitIsReadToTheLimitAndSaysSoAtTheTop` |
| 输出文本的大小 | 不按 `MaxOutput` 停 | `TestTheOutputIsBoundedAndTheDocumentIsCutAtAPageAndSaysSo` |
| 没有网络、不能起程序 | 不装 seccomp 过滤器 | `TestTheProcessCannotConnectOrStartAProgram`（`dial` 得到的是 connection refused，不是 operation not permitted；`/bin/true` 起来了） |
| 没有环境变量 | `cmd.Env` 用父进程的 | `TestTheProcessGetsNoEnvironmentFromTheService` |
| 不继承文件描述符 | 不调用 `closeInheritedFiles` | `TestTheProcessKeepsNoFileOfItsParentAndIsInTheRootDirectory`（父进程交下的 `/dev/null` 在子进程里还开着） |
| 并发上限 | 槽数改成 1000 | `TestWhenAllTheSlotsAreTakenACallWaitsAndThenSaysBusy`（第二个调用没有得到 `pdf_busy`，而是跑了起来） |
| 子进程没说怎么读的、或崩了，不当作成功 | 去掉 `parseInfo` 的校验 | `TestACrashOfTheParserCostsOneCallAndTheNextOneWorks`（"a reader that says nothing about how it read: <nil>"） |
| 解析器的 panic 是错误不是崩溃 | 去掉 `recover` | `TestAPanicOfTheParserIsAnErrorAndNotACrash` |
| 文字层规则：十分之一的页 | 去掉份额判断 | `TestTheRuleForATextLayerIsTwentyCharactersOnOneTenthOfThePages`、`TestAScanWithAStampOnOnePageHasNoTextLayer` |
| 文字层规则：每页 20 个字符 | 改成 1 个 | 同上 |
| 页标记 | 不写 `[page N]` | `TestEachPageHasAMarkerTheModelCanCite` |
| 加密的单独报错 | 不识别 `ErrWrongPassword` | `TestAnEncryptedDocumentIsRefusedOnItsOwn` |
| 看字节判断 PDF | 只有标成 PDF 的才交给读取器 | `TestABodyThatStartsWithTheHeaderIsAPDFWhateverTheServerCalledIt`（octet-stream 被拒绝，text/plain 与 text/html 把 PDF 的源码当页面交给了模型） |
| 标成 PDF 的 HTML 页面当 HTML 读 | 一律 `pdf_malformed` | `TestABodyLabelledPDFThatIsAnHTMLPageIsReadAsTheHTMLPageItIs` |
| 不能装 PDF 的类型不读正文 | 所有类型都读正文再判断 | `TestTypesThatCannotHoldAPDFAreRefusedUnreadWhateverTheBody` |
| 没配读取器：PDF 不可用 | 总是去调读取器 | `TestWithoutAPDFReaderAPDFIsAnsweredThatReadingIsNotAvailable` |
| 启动自检 | 不调用 `Check` | `TestACommandThatCannotReadFailsTheSelfTestAndTheReaderIsOff`（`/bin/true` 起得来、什么也不读，读取器却开着） |
| `IDP_WEB_PDF=off` | 忽略它 | `TestPDFReadingCanBeSwitchedOffAndAWrongSettingIsAnError` |
| 每个错误有自己的句子与原因 | `pdf_no_text_layer` 的一行改成 `internal` | `TestEverySentinelHasAFixedSentence`、`TestEachRefusalOfThePDFReaderHasItsOwnSentenceAndReason`（"counted map[failed/web_open_url/internal:1 …]"） |
| 首行写明 PDF、总页数与是否被截断 | 去掉 PDF 的首行分支 | `TestAPDFIsHeadedAsOneAndPagedLikeAnyPageAndCitedByItsPart`、`TestACutPDFSaysItIsCutInTheHeadAndTheModelDoesNotHaveToOpenTheText` |

**没有做反向验证的：**`FindCommand` 的几种检查（目录、不可执行的文件）只有正向测试；seccomp 的名单里每个调用没有逐个去掉（只量了"整个过滤器"）；`RLIMIT_CPU`、`RLIMIT_NOFILE`、`RLIMIT_FSIZE`、`RLIMIT_CORE` 没有各自的测试（`RLIMIT_CPU` 在"子进程自己的期限"一行里间接起了作用）；容器里的 seccomp 档案是否放行 `seccomp(2)` 只在本机 Docker 的缺省档案下验证过，没有在 Kubernetes 的 `RuntimeDefault` 上验证。

## 反向验证与细节（D32，Happy Eyeballs）

**为什么。**idp-web 的读取器对一个名字检查完所有地址后，原来按解析器给的顺序一个接一个地拨。在 IPv6 不通的网络里，第一个地址若是 IPv6，就要等它超时才轮到 IPv4。真实的例子是 www.fedex.com（解析结果先给 2600:1417:… 的 IPv6 地址）：在没有 IPv6 路由的网络上，IPv6 超时，IPv4 0.5 秒就答。

**做法。**`dialRace(ctx, dial, ips, port)`（`internal/fetch/dialrace.go`；三个仓库各抄一份，不共享代码，与地址规则一样）：先交错地址族（RFC 8305 第 4 节；族内保持解析器的顺序；从哪个族开始由下面的地址族设置定，默认 IPv4），第一次尝试立即开始；一次尝试在 `connectionAttemptDelay`（250 ms，具名常量）内既没连上也没失败，就在它继续的同时开始下一个；失败的立即开始下一个（第 5 节）；先连上的赢，其余尝试被取消，晚到的连接被关闭；全部失败返回最后一个错误，调用处的错误包装与审计原因不变。拨号函数是接缝，测试注入行为。超时：沿用请求的上下文（15 秒）作为总期限；没有单次尝试的上限。

**没有变的。**地址检查（有一个地址不是公网就整体拒绝，发生在任何拨号之前）、不重新解析（只拨检查过的 IP，不拨主机名）、配置了代理时只拨代理。没有做的：AAAA 与 A 并行解析（RFC 8305 第 3 节），解析仍是一次。

**反向验证（2026-11-24）。**每项去掉后，下列测试失败，恢复后全过：

| 去掉 | 失败的测试 |
|---|---|
| 不错开（计时器到点不启动下一个） | TestHangingFirst…、TestReadGetsThePageThroughTheSecondAddress… |
| 不取消落败的尝试 | 同上两个 |
| 不关闭晚到的连接 | TestAConnectionThatCompletesAfterTheRaceIsClosed |
| 不交错地址族 | TestInterleaving… |
| 在已检查的地址前多拨一个未检查的地址 | TestReadGetsThePage…，以及 TestANameWithAPrivateAnswer…、TestRebinding…、TestEveryConnectionIsResolvedOnce… 等 6 个 |

**地址族设置：`IDP_WEB_DIAL_FAMILY`（后补，同日）。**真实检查发现一种连接竞速看不见的情况：从没有可用 IPv6 的网络读 FedEx（Akamai）香港的页面，IPv6 的 TCP 有时在 250 ms 内就连上，随后数据不通（curl -6：状态 200 之后什么都没有，直到超时；curl -4：0.24 s）。解析器按 RFC 6724 把 IPv6 排在前面，于是 IPv6 先开始、先连上、赢了，然后卡住；连接竞速只比谁先连上。所以加一个设置，取值 `prefer-ipv4`（**默认**）、`dual`（RFC 8305 原样：解析器的第一个族先拨）、`ipv4-only`、`ipv6-only`；`prefer-ipv4` 是同一个竞速，只是交错从 IPv4 开始；`ipv4-only` / `ipv6-only` 在竞速之前丢掉另一个族，一个都不剩时报与“无地址”相同的连接错误。未知值是启动错误，启动时记一行日志。**为什么默认 `prefer-ipv4`：**大陆和许多企业网络的 IPv6 连得上却会卡住，而 IPv4 几乎在所有地方都通；代价只在纯 IPv6 的网络：IPv6 的尝试晚 250 ms 才开始（这样的网络用 `dual`）。检查、不重新解析、代理都没有变。

| 去掉（地址族设置） | 失败的测试 |
|---|---|
| `prefer-ipv4` 也从第一个地址的族开始（等于 `dual`） | `TestReadUsesIPv4WhenIPv6ConnectsAndThenStalls`、`TestPreferIPv4NeverLetsAStallingIPv6PathWin`、`TestEachFamilySettingGivesItsOrder` |
| `ipv4-only` 不丢掉 IPv6 | `TestEachFamilySettingGivesItsOrder`、`TestAFamilyWithNoAddressIsAnErrorWithoutDialing` |
| 去掉验证 | `TestDialFamilySettingIsValidated`；启动时 `fetch.ParseDialFamily` 出错即 `fatal`，并记一行日志 |
