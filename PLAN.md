本计划用于实现 CLIProxyAPI（CPA）的原生动态库插件 **cpa-vibeguard**，完成模型请求中的文本脱敏，以及响应文本和工具参数的还原。插件 ID、配置键、元数据名称及动态库基名统一为 `cpa-vibeguard`。当前阶段交付设计与实施计划，后续实现以本计划为依据。

已确定采用 CPA plugin，不修改 CPA 宿主。尽量覆盖 CPA 已有 API 格式，Gemini 与其他主要对话协议一起纳入首版。规则支持原生 YAML inline，以及兼容 Gitleaks 的 TOML 外部文件和同结构 YAML 外部文件，可按 ID 统一禁用内置或自定义规则；保留环境和请求变量、会话内稳定标记、多来源 session fallback，以及可选的未支持格式放行。协议由 CPA 提供，插件内部适配，不提供 api 选择配置。增加可配置的请求头跳过条件；路径判断由前置 Caddy 反代完成并注入请求头，首版插件不增加路径前缀跳过配置。

已接受的流结束行为是：完整标记收齐即还原，正常协议结束时处理残缺尾部；上游突然中断或未发送结束事件时，丢弃插件尚未输出的残缺标记和解析缓冲，释放请求状态，不尝试在 `request.complete` 中补发正文。此行为无需修改宿主。

实现采用 Go 核心加薄 C ABI 接入层，以 CPA 的 [request-lifecycle 示例](/tmp/CLIProxyAPI/examples/plugin/request-lifecycle/README.md:1) 为接入起点。原 `/data/workspace/caddy-llm-privacy-filter` 仅作为 Gitleaks 规则处理、准确定位问题和请求样例的参考；新项目不依赖其 HTTP 模块、指令解析或 Replacer。

兼容性起点：本地 `/tmp/CLIProxyAPI` 提交 `09a29bd345bc44c473abe7fd07859e32df2ea543`，Go 1.26，CPA C ABI 1 / RPC schema 6，Gitleaks v8.30.1；规则参考项目提交 `d645e24`。依赖应固定到验证过的版本；发布不依赖 `/tmp` 的本地 replace。此前选型依据见 [PLUGIN-ASSESSMENT.md](/data/workspace/cpa-vibeguard/PLUGIN-ASSESSMENT.md)，发生差异时以本计划和用户后续约定为准。

已核对的接入事实如下。

| 源码 | 已确认行为 | 实施约定 |
| --- | --- | --- |
| [请求拦截器](/tmp/CLIProxyAPI/sdk/api/handlers/handlers_interceptors.go:467) | 有 RequestID、SourceFormat、请求头、正文和 metadata；改写同时更新 Payload 与 OriginalRequest | 在请求阶段保存映射，不依靠响应阶段的 OriginalRequest 找回秘密 |
| [请求 metadata](/tmp/CLIProxyAPI/sdk/api/handlers/handlers.go:196) | 可携带 caller_scope、execution_session_id、request_path；路径优先使用 Gin 路由模板 | 身份和 session 使用明确来源；不假设一定有原始 URL 或完整 HTTP context |
| [能力声明](/tmp/CLIProxyAPI/internal/pluginhost/rpc_schema.go:35) | 请求、普通响应、流块、请求完成有独立能力 | 注册四项拦截/生命周期能力，不替换 executor、认证或 translator |
| [流块返回值](/tmp/CLIProxyAPI/sdk/pluginapi/types.go:1169) | 可替换 Body 或 DropChunk；空 Body 表示保留原块 | 需要暂扣整个块时显式 DropChunk；优先只暂扣目标字段的必要尾部 |
| [完成通知](/tmp/CLIProxyAPI/internal/pluginhost/adapters_interceptors.go:189) | request.complete 异步执行，不能返回响应正文 | 仅清理状态与 lease；异常断流丢弃未输出缓冲 |
| [插件故障调用链](/tmp/CLIProxyAPI/internal/pluginhost/adapters_interceptors.go:18) | RPC/拦截器错误被跳过；可捕获的 panic 可导致插件熔断 | 已处理的请求错误返回成功 RPC 内的 Terminate；接受宿主故障放行边界 |
| [SDK 格式](/tmp/CLIProxyAPI/sdk/translator/formats.go:3)、[API 路由](/tmp/CLIProxyAPI/internal/api/server_routes.go:61) | 除 OpenAI、Claude 外还有 Gemini、Interactions 和多个专用执行入口 | 按实际执行协议、字段和回调路径建立覆盖矩阵，不只判断 messages |

实施中的固定约定如下。

1. **原生插件接口。** 构建 `cpa-vibeguard.so`，首个验证平台为当前 Linux；使用 `CGO_ENABLED=1` 与 `go build -buildmode=c-shared`。注册 `request_interceptor`、`response_interceptor`、`response_stream_interceptor`、`request_lifecycle_plugin`。实现 `plugin.register`、`plugin.reconfigure`、`plugin.quiesce`、关闭入口，以及 `request.intercept_before`、`request.intercept_after`、`response.intercept_after`、`response.intercept_stream_chunk`、`request.complete`。使用公开 `sdk/pluginapi`、`sdk/pluginabi` 类型，薄层负责 RPC envelope、JSON/base64 编解码和 C 缓冲分配/释放，业务核心不依赖 Gin 或宿主 internal 包。配置声明 schema 6，并检查宿主版本；不为旧 schema 的逐块请求/历史复制增加兼容分支。

   主要脱敏在 `request.intercept_before` 完成；这里的 before-auth 是上游凭据选择之前，入站认证已经执行。`request.intercept_after` 默认保持当前请求，只核对和复用已建立的请求状态，不因重试或凭据切换重新选择会话、生成标记或追加提示。同一 RequestID 的重复调用必须幂等。不同 RequestID 即使同属一个 TraceID 也分别持有状态；会话复用通过独立的 session scope 完成。

2. **协议覆盖。** 首版的目标是覆盖 CPA 可达的文本生成、消息、工具调用和对应 JSON/流式响应，而非仅实现最初三个协议。按以下矩阵逐项提供真实宿主加 mock upstream 的样例；SDK 的上游格式名和客户端 API 不是一一对应，不能按 provider 名称重复实现协议转换。

   | 协议或入口 | 目标处理范围 | 适配方式 |
   | --- | --- | --- |
   | OpenAI Chat Completions、旧 Completions | 消息、prompt、工具参数、文本输出和流式 choices | `openai`；旧入口若在钩子前已转为 Chat，复用宿主实际交付的结构，并验证最终旧式响应 |
   | OpenAI Responses、Codex 路由别名、compact | instructions、input、输出项、函数调用/结果、增量与完整快照 | `openai-response`；处理普通 JSON、SSE 和 compact 中可解释的文本，保留 opaque compaction 数据 |
   | Anthropic Messages 与 count_tokens | system、content blocks、tool_use/tool_result、文本和工具参数增量 | `claude`；计数结果保持数值，仅对送往计数器的消息执行同样脱敏 |
   | Gemini generateContent、streamGenerateContent、countTokens | systemInstruction、contents/parts、functionCall.args、functionResponse.response、candidates | `gemini`；支持对象形式工具参数、finishReason、SSE 及宿主生成的 JSON 流封装 |
   | Gemini Interactions | input、可解释的 steps/outputs、工具调用与结果、事件流及续接引用 | `interactions`；独立字段/事件适配，不因存在 input 就误认成 Responses |
   | SDK Codex、Antigravity 格式 | Responses 类数据或 Gemini 外层 request 包装中的上述文本字段 | 在这些格式实际进入钩子时按结构适配；作为 ToFormat 上游时沿用宿主翻译，不二次转换 |
   | Images、Videos、Codex alpha search | 钩子内可见的 prompt、查询、文本结果和 revised_prompt 等明确文本字段 | 为 `openai-image`、`openai-video`、`codex-alpha-search` 及其内部转换路径建立描述；媒体本体保持不变 |
   | Responses WebSocket | 已走 ExecuteStreamWithAuthManager 的每轮模型请求和输出事件 | 复用 Responses 适配与 execution_session_id；验证同一连接多轮、断开、native/fallback 路径 |
   | 模型列表、管理、认证、媒体下载、Realtime/Live 等其他入口 | 保持 CPA 已有服务行为 | 对未进入这些拦截器的路径不宣称脱敏；能进入且有明确文本结构的扩展逐项适配，其余按未知协议策略处理 |

   图片、音频、视频二进制和 base64、签名、加密 thinking/compaction 内容不执行字符串替换，也不新增 OCR/转写。保留已知多模态节点及非文本字段，在其可解释的文本部分脱敏。未知格式的兼容放行与“已经完成脱敏”分别记录。发布时提供实测覆盖矩阵，明确有限支持和未进入钩子的入口；不将未测试的全部 CPA API 标为已支持。

3. **内部协议适配与未支持格式。** 不提供 `api` 配置，也不实现独立的通用协议探测器。CPA 已通过请求钩子的 SourceFormat 指定输入格式，插件按该值直接查找内部适配器。旧 Completions 等入口若已被宿主归一化，就处理其实际交付结构。同一 SourceFormat 下的 count/compact 等操作由已知正文结构、宿主 operation 信息或 metadata.request_path 辅助区分；不遍历所有协议识别器，不根据模型名称或正文猜测并覆盖宿主协议。

   检测规则、span 合并、标记生成和映射存储不区分协议；适配器仅负责提取/写回可修改的字段、工具参数 codec、输出标识和结束事件。例如，Chat 的工具 arguments 常是内含 JSON 的字符串，Gemini functionCall.args 是对象；Claude 和 Responses 也有不同的流结束事件。仍需这层差异处理，避免对所有 JSON 字符串统一替换而改坏模型/工具名称、签名或媒体数据。适配器对选定结构做必要校验；非法 JSON、重复成员名、混合互斥结构、超限及已支持操作的非法消息形态属于处理错误，不切换到别的协议尝试。

   保留 `on_unknown_api: reject|passthrough`，默认 reject，仅控制 SourceFormat 缺失/未注册，或明确尚未实现的完整 API 操作。已支持操作中新出现的未知消息节点不能因此默认为已扫描，应返回明确的处理错误。显式 passthrough 时整次请求和响应不改写、不建立原文映射；放行是交还 CPA 原有流程，不新增任意路径反代。已经开始处理后的规则、会话、解析或还原错误使用各自故障策略，不能改判成未知格式。

   响应通过 RequestID 找到固定状态，使用宿主响应回调的 SourceFormat 选择对应解码器，并验证与当次执行上下文一致；不对正文重新猜测协议或重跑请求规则。协议覆盖矩阵与相关分支属于内部实现，日常配置无需列举或选择 API 类型。

4. **按请求头显式跳过。** 配置 `bypass.headers` 为有序条件数组，默认空数组；任一条件命中即跳过。条件使用 `name` 加 `value` 精确匹配，或显式 `present: true` 表示只要求头存在，两种条件不得混用。头名称大小写不敏感，值大小写敏感；value 模式要求恰好一个值且完全相等，不自动 trim、拆分逗号或把任意真值当成 true。缺失或不匹配继续正常处理；多值头不能误触发精确值条件。加载时拒绝非法头名和有歧义的条件。

   在第一次请求钩子里，先取配置和请求头快照，再判断 bypass；命中后不解析业务正文、不绑定规则变量、不选择协议适配器、不提取 session、不扫描或追加提示。保存仅含 RequestID 和跳过原因的有界轻量状态，后续请求钩子、普通响应和所有流块沿用这一决定，不进行还原；正文即使包含已知格式标记也保持不变。request.complete 幂等清理该状态。未支持格式放行与 header 跳过使用不同状态/指标。

   跳过规则只影响插件，CPA 认证、路由和此前已执行的校验仍生效。插件不创建绕过路径，不重写 URL，不默认信任 X-Original-URI 等头。路径前缀选择由前置代理注入约定的跳过头完成，示例见后文。

5. **规则兼容与来源。** 使用 Gitleaks Go 库，默认加载官方内置规则。`rule_sources` 是有序来源数组，每项恰好为 `file` 或 `inline`。inline 直接填写 YAML mapping；外部 `.toml` 文件读取 Gitleaks TOML，`.yaml`/`.yml` 文件读取同结构的 YAML 规则文档。扩展名不区分大小写，未知扩展名报错；选定解析器后遇到错误直接报错，不轮流尝试其他格式。文件使用绝对路径，通过重新配置或重启重读，首版不加载远程订阅。

   inline 的对象与外部 YAML 文件内容完全同构，包含 `rules` 数组及可选的 extend、全局 allowlists 等；把整个 inline 对象移入 YAML 文件不改变语义。不接受 `inline: |` 中嵌入 TOML/YAML 文本，避免同一配置叠加两层文档转义。TOML 与 YAML 只是不同输入表示，都直接解码为同一 RuleDocument，再统一校验、合并、绑定变量及构造 Gitleaks 检测器；不先把 YAML 拼成 TOML 再解析，也不维护两套规则执行逻辑。

   YAML 使用 Gitleaks 原字段名，例如 secretGroup、regexTarget、targetRules、useDefault、disabledRules，不另设 snake_case 别名；TOML 的 `[[rules]]` 对应 YAML 的 `rules` 列表。支持 id、description、regex、secretGroup、entropy、keywords、tags，全局/规则级 allowlists，以及 condition、regexTarget、stopwords、targetRules。keywords 保持预筛选含义。首版支持 v8 文本规则；仓库路径、commit、递归 extend 和未覆盖的 required/skipReport 等字段给出明确错误。旧式单数 allowlist 转成统一表示，与同层复数并用按 Gitleaks 约定报错；官方内置配置采用独立加载路径。YAML 规则格式由本插件提供，不要求原 Gitleaks CLI 支持 YAML。

   所有来源按 rule_sources 声明顺序合并，与格式无关。同一文档重复 rule ID 报错，后续文档仅覆盖之前同 ID 规则中显式提供的字段，allowlists 按既有约定追加。统一表示保留字段是否出现的信息，区分未提供与显式 false、0、空数组，避免 TOML/YAML 解码默认值改变覆盖结果。可对已有 rule ID 仅追加 allowlist，引用不存在 ID 报错。useDefault 使用最后一个显式声明，默认 true。PII 提供独立可选规则包。错误只报告来源路径或 inline 序号、字段位置和规则 ID，不回显规则正文。

   插件顶层提供 `disabled_rules` 字符串数组，默认 `[]`，可列出任意内置或自定义规则的 ID。它与所有来源的 `extend.disabledRules` 取并集并去重，在规则合并和静态校验完成后、变量求值和构建运行时检测器之前统一移除对应规则。后续文件或 inline 的同 ID 定义不能重新启用它，空列表也不清除其他来源的禁用项；重新启用须从所有禁用列表移除该 ID 并重新配置。ID 区分大小写、精确匹配，不支持通配符或正则；空 ID、非字符串及合并后不存在的 ID 在加载时明确报错。引用检查使用禁用过滤前的完整规则集合，targetRules 指向已禁用 ID 本身不算引用错误。同一文本仍可被其他启用规则命中。

   禁用不豁免来源解析、字段类型、模板语法及可静态发现的规则错误；仅为最终启用的规则及其适用的 allowlist 建立变量依赖并求值。只被禁用规则使用的环境或请求变量不要求提供，共享变量仍由启用规则的实际依赖决定。面向已禁用规则的 allowlist 不参与扫描；全局 allowlist 按剩余启用规则计算适用范围。

   YAML 正则推荐单引号，以原样保留 `\b`、`\d` 和反斜杠；多行文本可用 `|-` 明确移除末尾换行。解析后严格校验字段与类型，不把数字、布尔值、null 自动当成 regex/keywords 字符串。插件直接读取的 YAML 文件只允许一个规则文档，重复键及未知字段报错；inline 校验针对 CPA 实际传入的 YAML 结构。两种格式使用同一套规则兼容和冲突判定。

6. **环境和请求变量。** 保留 `{env.NAME}`，由插件读取 CPA 进程环境；请求变量改为 `{var.NAME}`，在 `variables` 中显式声明来源，每项恰好为 env、header、json、metadata 之一。json 和 metadata 使用 RFC 6901 Pointer。请求级来源取第一次钩子的头、脱敏前 JSON 和 metadata 快照；环境值在配置快照生成时读取。所有求值结果固定至该次请求完成，响应还原不重新读取。

   先将 TOML/YAML 来源解析成统一规则结构并完成合并、禁用过滤，再仅在启用规则的 regex/keywords 和适用 allowlist 的 regexes/stopwords 中展开模板。regex 中按字面量转义并包非捕获分组，keywords/stopwords 使用原始字符串。识别器区分变量、数字量词、Unicode 类和转义花括号，禁止在字符类或未完成转义处插值。对于启用规则实际依赖的变量，缺失、空值、非字符串、头部多值歧义及过长值均明确报错；无效或空匹配规则同样报错。rule ID、数值、规则来源路径等不做请求级展开。不实现 Caddy 占位符；配置和日志不写回变量求值结果。

7. **编译、定位与并发。** 静态规则在加载期严格校验，只在动态绑定变化时重新编译相关部分；缓存按配置代次和绑定值构建不可逆键，限制容量、内存和 TTL。配置、编译结果和规则快照发布后只读，请求不得改写共享检测器。动态 allowlist 必须同样作用于静态规则命中。

   Gitleaks 适配器返回准确 UTF-8 字节 span 和规则元数据，覆盖多行、CRLF、中文、重复原文、捕获组、交叉和包含命中。显式设置 IgnoreGitleaksAllow=true；请求文本中的 gitleaks:allow 不改变检测结果。关闭额外 base64/URL 解码；无法可靠定位、无效规则或取消的部分扫描返回错误。相交 span 取并集，规则排序确定，单次拼接替换。参考项目中捕获组定位、重叠范围漏替换、无效正则静默跳过的问题均纳入回归样例。

8. **标记和映射。** 固定格式为 `[[redacted:v1:<id>]]`，id 是加密安全随机生成的 128 位值，以 32 位小写十六进制表示；完整标记长 48 个 ASCII 字节，不携带原文片段或规则名。同一作用域的相同 UTF-8 原文字节复用标记，不做大小写或 Unicode 归一化；检查碰撞，并发分配同一原文得到同一标记。默认请求隔离，显式 session 配置才启用跨请求复用。

   独立 MappingStore/lease 管理容量、TTL 和活动引用。请求固定持有 scope，活动请求不能被普通 TTL/LRU 淘汰；资源不足时按请求错误处理。RequestID 索引只保存还原必需信息，不将完整原始 prompt 留作历史。已生成的脱敏请求可受限缓存用于幂等处理。输入中看起来像标记的文本不因此免于扫描，未知标记不跨作用域查询。首版不持久化秘密映射。

9. **session 来源和隔离。** 可选 `session` 块中，identity 固定采用 CPA 已认证调用方形成的 `metadata.caller_scope`，不接受客户端身份头替代。`profiles` 是有序数组，每项配置 namespace 和有序 sources，可用 source_format、requested_model_prefix 选择；条件同时满足算命中，首个 profile 生效，无 match 表示兜底。未命中任何 profile 的普通请求使用请求模式；依赖历史引用的请求仍需验证其会话。选择只发生一次，不根据后续选中的凭据或 ToFormat 改变。

   每个 source 恰好为 header、json、metadata 之一；后两者用 JSON Pointer，支持转义和数组下标，不引入 JSONPath。依次选第一个有效非空字符串；只有缺失、null、空字符串才能 fallback。非字符串、多值歧义、控制字符、首尾空白或超过 max_id_bytes 的值报错；不做隐式转换、去空格或额外解码。多个来源都有值时首项生效，不建立不同 ID 的别名；命中 ID 尚无映射也不能换下一个来源或 profile。来源类型和序号不进入会话键。

   会话键无歧义编码 `(namespace, caller_scope, session_id)`。候选 ID 存在而可信身份缺失时拒绝；所有来源缺失且无续接依赖时执行 `on_missing: request|reject`，默认 request。max_id_bytes 默认 1024。默认空闲 TTL 30m，并限制会话数、单会话映射数和总字节数；TTL 仅回收无活动 lease 的会话。来自 header/JSON/metadata 的相同 ID 在相同身份和 namespace 下复用映射，不同身份和 namespace 相互隔离。内置 derived/canonical session 仅在用户显式配置来源时使用，不自动替代本插件来源规则。

   Responses previous_response_id、Interactions previous_interaction_id 等历史引用不能直接作为稳定 session source。适配器登记响应 ID 到已认证会话的绑定；显式 session ID 与历史引用必须一致，无来源但绑定有效可沿用，未知/过期/冲突引用拒绝，不降级为独立请求。Gemini cachedContent 等宿主未暴露完整创建链的服务端缓存作为明确的有限支持项，不自动当会话 ID，也不承诺还原未登记的历史标记。进程重启、二进制替换和映射过期后，需要重建上下文或明确拒绝依赖旧映射的续接。

10. **缓存稳定性。** 会话内首次随机分配、后续请求复用；重发相同历史时保持脱敏后的前缀稳定。请求模式、会话失效、重启或检测范围变化可改变标记，影响第一个变化位置之后的 prompt/KV 前缀复用，之前的前缀仍可能命中。缓存条件和用量字段须由对应上游实测，本地 mock 只验证输入稳定性。prompt_cache_key 不能抵消正文变化；外部缓存脱敏后的回答还必须绑定可还原的映射范围和生命周期。

   后续如需跨会话/进程稳定性，可显式引入带服务端密钥和已认证作用域的 HMAC 标记策略；不使用无密钥原文哈希或跨调用方通用标记。可重算 ID 不代表能找回原文，此扩展不是首版前置条件。

11. **结构化处理。** 对钩子实际交付的 JSON 进行精确字段遍历，保留数值精度，拒绝重复成员名；已选适配器、变量和 session 复用同一解析树。文本字符串、字符串内的工具 JSON、Gemini 等对象形式参数使用对应 codec。覆盖指令、消息、工具调用/结果、明确的工具描述、文本输出及协议规定的快照，不递归改写所有 JSON 字符串。协议路由字段、模型/工具名称、签名和媒体载荷按类型保留，避免破坏宿主转换。

   宿主请求正文可能已经做过入口归一化，响应可能尚待外层包装，必须按接口阶段建立样例。请求脱敏在上游转换前完成；不因 ToFormat 是 codex、antigravity 等就再次扫描成另一种输入协议。计数 API 会对脱敏后的内容计数，标记本身会影响 token 数量，文档明确这一行为。

12. **精确还原与引用提示。** 对当前映射授权范围内完整且精确匹配的标记进行单次替换。还原后不递归扫描，避免原文恰含标记时二次替换。裸 ID、部分 ID、大小写变化、插空格、未知或其他作用域标记都不猜测原文。正常返回中可识别的未知/残缺标记保留原样；异常流结束的未输出尾部按第 13 条丢弃。

   `preserve_marker_hint` 可选，默认 false；启用时在协议允许的指令字段中加入固定提示，要求完整复制双层括号、前缀与 ID，不缩写或改写，不覆盖既有指令且每次请求只添加一次。提示不包含请求随机值。标记不是 tokenizer 的不可分割 token，提示不能保证模型完整引用；文档保留这一准确边界。

13. **流式还原与结束。** 区分宿主交付的裸 JSON chunk、SSE 帧及其他已支持的流封装，不把整个 SSE 字节串作正则替换。通用 SSE 解析处理 LF/CRLF、多行 data、注释和分块；协议适配器区分文本 delta、工具参数 delta、完整快照、控制和结束事件。状态按 RequestID、输出/候选、内容块、工具调用及字段分别隔离；完整快照独立还原，不能与 delta 重复拼接。

   收齐完整标记立即还原；确定不是标记的文字立即输出；普通文本仅缓存可能构成标记的必要尾缀，每字段最多 47 个 ASCII 字节。工具 JSON 使用增量 codec 和独立上限，不能缓冲整条回答后宣称只占 47 字节。优先改写字段并保留同块的其他数据；只有当前块没有可交付内容时才 DropChunk，不丢失并行输出、角色、usage 或控制信息。schema 6 不依赖 HistoryChunks，也不要求每个块重新附带原请求。

   在对应字段结束事件到达时排空缓冲，再发结束信号：Chat 的每 choice finish_reason、Claude content_block_stop、Responses 的文本/参数 done 事件、Gemini 每 candidate finishReason、Interactions 对应的内容和交互终止事件。不能等内容块已经关闭后才追加 delta；Chat 需返回合法 JSON，SSE 格式可合成保持顺序的合法帧，不能统一拼接多个裸 JSON。完整快照与最终聚合响应同样还原，避免末尾又出现标记或重复原文。

   **异常断流丢弃尾部是已确认需求。** EOF/取消/上游错误没有再次调用正文钩子时，不补发、不猜测残缺标记；request.complete 丢弃未输出缓冲并释放 lease，已交付结果不回滚。普通文本的上述上限不包括未完成 SSE 帧、JSON 转义和工具参数缓冲；这些也各自有上限，异常时允许丢弃。客户端主动断开时不继续发送。不能依赖宿主在外层生成的 `[DONE]` 再获一次插件调用。

14. **故障策略。** `on_request_error: reject|passthrough` 默认 reject，约束插件已经接管且能正常返回的请求处理错误。reject 使用成功 RPC envelope 中的 Terminate、自定义状态及不含正文的简短错误；配置/规则/变量/session 错误不静默当作未命中。显式 passthrough 必须在任何改写提交之前返回本次钩子的原始正文，丢弃本请求尚未发布的临时映射并标记整次跳过，不删除其他请求已使用的会话映射。上游重试始终使用已经提交的脱敏请求，不在中途退回原文。

   普通响应还原按整体事务处理，失败时保持 CPA 当前响应并记录降级。响应找不到 RequestID 状态时保持当前正文，不搜索其他会话映射。流式本地解析/还原失败且无法继续时，丢弃未输出缓冲，将当次响应标为停止还原，后续块由宿主照常处理；不重复发送之前暂扣或已发送的原始块，不伪造成功终止事件。正常完成的已支持输入保证结构正确；上游截断、畸形数据或降级不承诺完整工具参数。现有接口不能通过返回 error 强制停止流。

   接受宿主 RPC 故障、熔断、插件未加载时可能跳过插件的现状，不再提供暗示端到端 fail-closed 的 fail_open 开关，不将强制插件或流结束接口改造列为任务。业务拒绝、显式 header 跳过、未知协议放行和宿主故障是不同结果。

15. **配置更新与生命周期。** `plugin.register/reconfigure` 严格解析 CPA 传入的 config_yaml，允许宿主保留键 enabled/priority；其他未知插件字段报错。新配置完整校验后原子发布，失败保留旧快照。活动请求固定持有旧快照和 lease，不被重配置清空；规则变化时令编译缓存按代次失效，兼容的会话映射可继续使用。修改 disabled_rules 或 extend.disabledRules 只改变新请求的检测集合，不删除已有标记的还原映射；在途响应和同会话历史中的完整标记仍按映射生命周期还原。

   request.complete 对成功、失败、拒绝和取消均幂等处理，不依赖回调的正文写入能力；已拒绝/已清理 ID 不重复释放。请求状态、跳过状态、缓冲和映射均有容量上限；达到容量时停止接收新的受保护请求，不能无限增长或淘汰活动请求。关闭时释放所有本实例资源。实现 quiesce 以配合宿主生命周期，但不承诺依靠该回调在二进制热替换时迁移内存映射。二进制升级以排空在途请求并重建需要的会话为运行约定，配置热更新与二进制替换分别验证。

16. **资源与日志边界。** 限制插件可见请求/普通响应字节数、单事件大小、未完成工具 JSON、单请求状态、并发请求、规则缓存及 session 映射总量。插件开始执行前宿主已经读取或归一化的 HTTP 正文不在这些限制覆盖内；HTTP 压缩、Content-Length、连接、写入和 Flush 仍由 CPA 负责。

   插件日志仅记录请求处理状态、规则 ID、配置来源位置、来源序号及无正文的错误码，不输出原文、完整标记、动态正则、身份/session 值或工具参数。配置序列化仅含模板与声明的静态配置。CPA 自身可能先采集请求正文或保留错误日志，这是已接受的宿主行为；项目只保证本插件的日志约定，不把改宿主日志作为前置工作。

建议的目标 CPA 配置如下。`plugins.enabled/dir/configs` 及单插件 enabled/priority 是宿主已有字段；其余是本插件待实现的配置。示例包含可选变量规则和 session，最小配置可以省略这两部分。

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    cpa-vibeguard:
      enabled: true
      priority: 100
      on_unknown_api: passthrough
      on_request_error: reject
      preserve_marker_hint: true

      bypass:
        headers:
          - name: X-Vibeguard-Skip
            value: "true"

      limits:
        request_bytes: 8388608
        response_bytes: 8388608
        event_bytes: 1048576
        tool_json_bytes: 1048576
        request_state_bytes: 16777216
        max_inflight: 256

      variables:
        tenant:
          header: X-Tenant

      disabled_rules:
        - generic-api-key
        - tenant-label

      rule_sources:
        - file: /etc/cpa-vibeguard/team.toml
        - file: /etc/cpa-vibeguard/custom.yaml
        - inline:
            rules:
              - id: internal-project-code
                regex: '\bPROJ-[A-Z0-9]{8}\b'
                keywords: ['PROJ-']
              - id: deployment-secret
                regex: '{env.PROJECT_SECRET}'
                keywords: ['{env.PROJECT_SECRET}']
              - id: tenant-label
                regex: '{var.tenant}'
                keywords: ['{var.tenant}']

      session:
        identity: caller_scope
        on_missing: request
        max_id_bytes: 1024
        ttl: 30m
        max_sessions: 1024
        max_entries_per_session: 4096
        max_total_bytes: 67108864
        profiles:
          - namespace: upstream-a
            match:
              requested_model_prefix: upstream-a/
            sources:
              - header: X-Upstream-A-Session
              - json: /metadata/session_id
              - metadata: /execution_session_id
          - namespace: default
            sources:
              - header: X-Session-ID
              - header: X-Conversation-ID
              - json: /session_id
              - json: /metadata/session_id
              - metadata: /execution_session_id
```

示例显式启用未支持格式放行和固定引用提示；省略对应项时分别默认为 reject 和 false。limits 及 session 容量采用示例数值作为首版默认值；省略 session 时保持请求隔离。rule_sources 为空时仍按 useDefault 约定启用内置规则，disabled_rules 仍可禁用其中的指定 ID。示例禁用内置 generic-api-key 和 inline 中的 tenant-label；这些规则定义可以保留。文件须预先存在，启用的 deployment-secret 需要 PROJECT_SECRET；tenant-label 已禁用且示例中没有其他规则依赖 tenant，因此无需 X-Tenant，重新启用该规则后才要求此头。不使用的变量规则也可直接删除。header 跳过和未支持格式放行不绑定请求变量，但静态配置仍须有效。

外部 `custom.yaml` 的内容就是一份规则文档，与 inline 下的对象形状相同。例如：

```yaml
extend:
  useDefault: true
  disabledRules: ['generic-api-key']
rules:
  - id: internal-hostname
    description: Internal service hostname
    regex: '\b([a-z0-9-]+\.internal)\b'
    secretGroup: 1
    keywords: ['.internal']
```

TOML 文件使用相同字段的 Gitleaks 表结构；例如上面这份 YAML 等价于：

```toml
[extend]
useDefault = true
disabledRules = ["generic-api-key"]

[[rules]]
id = "internal-hostname"
description = "Internal service hostname"
regex = '''\b([a-z0-9-]+\.internal)\b'''
secretGroup = 1
keywords = [".internal"]
```

这三种来源在进入规则引擎前已统一；外部 YAML 顶层填写 extend/rules/allowlists 等规则内容，不再套一层 plugins、rule_sources 或 inline。规则文档沿用 Gitleaks 的 extend.disabledRules 字段；disabled_rules 仅属于插件配置。示例的 generic-api-key 同时出现在两处禁用列表中，按并集只禁用一次，重新启用须从两处都移除。

session profile 按声明顺序选择，再在该 profile 内按 sources 顺序 fallback；二者不能混为“找得到映射就换配置”。requested_model_prefix 针对 CPA 传入的客户端请求模型名称，可用于已有上游模型前缀；不是选择最终凭据。match 也可指定 CPA source_format，例如 gemini 或 interactions。同 namespace、身份、相同 ID 可以从 header 切换到 JSON 后继续复用；不同值不自动合并。

若需要“只要带此头就跳过”，可将某个条件的 `value` 改为 `present: true`。首版只做 header 条件，路径交给已有 Caddy 反代。例如下面让 `/unmasked/v1/...` 去掉 `/unmasked` 前缀后进入 CPA，并加上跳过头；正常路径去掉客户端自行携带的同名头。

```caddyfile
:8080 {
    handle_path /unmasked/* {
        reverse_proxy 127.0.0.1:8317 {
            header_up X-Vibeguard-Skip true
        }
    }

    handle {
        reverse_proxy 127.0.0.1:8317 {
            header_up -X-Vibeguard-Skip
        }
    }
}
```

这是外部反代配置，插件只读取 `X-Vibeguard-Skip`，无需获取路径前缀，也无需编译 Caddy 扩展。部署已有其他路由时将同样的 header_up 规则放入对应反代即可。

建议的代码分工如下；这些是目标文件，并非本次已经生成的实现。

| 位置 | 职责 |
| --- | --- |
| `go.mod`、`cmd/cpa-vibeguard/` | Go 模块、C ABI 导出、动态库入口和缓冲所有权；元数据名称固定 cpa-vibeguard |
| `internal/plugin/` | RPC 分发、注册/配置/生命周期、四类拦截能力、RequestID 状态机和宿主协议适配 |
| `internal/config/` | 严格 YAML、默认值、有序 rule_sources、disabled_rules、变量、header bypass、session profiles 和限额 |
| `internal/rules/` | TOML/YAML 解码到统一 RuleDocument、来源合并、按 ID 禁用、变量转义与绑定、Gitleaks 适配和准确 span |
| `internal/protocol/` | CPA SourceFormat 到内部适配器的映射、操作/字段描述、文本与工具 JSON codec、多模态和 native wrapper 处理 |
| `internal/session/` | profile 选择、来源 fallback、caller_scope、namespace、续接引用索引 |
| `internal/token/` | 固定标记、并发稳定复用、请求/会话映射、容量/TTL/lease |
| `internal/restore/` | 精确替换、流尾缀、SSE 与协议事件、每输出/工具独立状态、异常丢弃 |
| `testdata/`、`tests/integration/` | 每种 CPA API 的脱敏/还原成对样例、流录制、mock upstream、动态库装载与真实宿主调用 |
| `examples/`、`scripts/` | CPA YAML、规则文件、可选 Caddy header 注入示例，以及各平台构建/装载脚本 |

按以下顺序实施，每一步给出可检查的结果。

1. **插件骨架、配置和入口清单。** 创建独立 Go module 与 c-shared 入口，固定公开 SDK 依赖，实现四项能力和生命周期的最小贯通。实现配置校验、header bypass 和状态清理。在未修改的 CPA 上加载动态库，记录协议矩阵各入口实际交付的 SourceFormat、正文形态和回调序列，使用无敏感内容的 fixture；补足路由到适配器的对应表。交付能加载的插件和 CPA 配置样例，不以更改宿主作为测试手段。

2. **规则核心。** 实现内置、外部 TOML/YAML、原生 YAML inline 来源，统一 RuleDocument、字段出现状态、合并与严格校验，汇总 disabled_rules 与 extend.disabledRules 并过滤，再接启用规则的变量模板、编译缓存、Gitleaks span 适配和覆盖合并。复现并修复参考实现的 allow 注释、捕获组定位、重叠范围和错误规则跳过问题。交付独立规则测试，覆盖三种来源语义等价、混合来源覆盖顺序、禁用优先级与变量依赖裁剪、变量不改变文档结构/注入正则、动态 allowlist 和并发快照。

3. **主要协议请求与普通响应。** 同期完成 Chat/旧 Completions、Responses/compact、Anthropic、Gemini、Interactions，以及可复用的 Codex/Antigravity 包装描述。按 CPA SourceFormat 分发内部适配器并处理未支持格式，再完成请求作用域、标记替换、普通响应和对象/字符串形式的工具参数还原。接入计数、图片/视频 prompt 和搜索等已有文本入口；已知非文本数据明确保留。交付每个已支持入口的 JSON 往返和实际插件调用证明，无需额外的 api 配置或跨协议正文探测。

4. **会话和多轮。** 完成 profiles、有序 header/JSON/metadata fallback、认证身份隔离、TTL/容量、稳定标记、续接引用绑定和请求幂等。验证历史重发、Responses/Interactions 续接、不同来源切换、并发同原文分配、缺少身份/过期映射及 CPA 重试。交付多轮输入前缀一致性测试，不能只验证首次请求。

5. **流式与 WebSocket。** 按每种协议实现字段级 delta、工具参数增量、完整快照、结束事件及有界缓冲；验证 Gemini 的流封装与候选 finishReason、Interactions 事件，以及 Responses WebSocket 通过共享执行路径的多轮。异常 EOF/错误/取消统一丢弃未输出缓冲，生命周期只清理。交付跨每个标记拆分位置、并行候选/工具、正常结束、异常断流和 header 跳过的集成样例。

6. **发布验证与文档。** 运行插件单元/集成测试、race 和 go vet，构建真实 c-shared 产物并在参考 CPA 上装载；验证配置更新、二进制替换、重启和多请求并发。每个主要输入格式验证原生或兼容执行路径，并选择代表性的跨协议转换，确保不因翻译改写丢失标记。记录必要的检测、变量缓存和长流内存/延迟基准。交付版本兼容表、实测 API 覆盖矩阵、安装配置、会话与缓存说明、header 跳过和异常尾部丢弃行为；其他平台产物按实际验证情况发布。

验收应重点覆盖以下性质。

- 构建产物名和配置键均为 cpa-vibeguard；未修改的参考 CPA 能加载 ABI 1 / schema 6 插件，注册四项能力，完成一次请求、响应、流块及生命周期调用。未知配置项与错误规则可见；错误 reconfigure 不清空活动快照。
- header 条件默认不启用；名称大小写、值精确匹配、present 模式、缺失/错误值、多值以及多个条件的 OR 语义符合约定。绕过判断先于正文处理，重试和配置更新不改变当次决定；请求与普通/流式响应均不改写，即使含标记也不还原。外部代理前缀剥离和 header 注入例子可实际连通 CPA。
- 无需 api 配置即可按 CPA SourceFormat 选择矩阵中的内部适配器，共享 messages/input 结构不触发跨协议猜测。错误正文、重复 JSON 成员、已支持操作中的非法/未知节点按处理错误处理；SourceFormat 缺失/未注册或明确未实现的完整操作按 on_unknown_api 策略处理。header bypass、未支持格式放行与处理失败的结果可区分。
- 同一规则文档使用外部 TOML、外部 YAML 或原生 YAML inline 时，得到等价的 RuleDocument、命中 span、allowlist 和变量行为。混合来源按声明顺序覆盖，未提供字段与显式 false/0/空数组正确区分；旧/新 allowlist 写法与重复 rule ID 的校验不因输入格式而变化。
- disabled_rules 默认空；可禁用内置、TOML、YAML 和 inline 规则，与全部 extend.disabledRules 去重取并集，后续覆盖或空列表不重新启用。未知/空/非字符串 ID 报错，大小写精确匹配；规则禁用后不再命中，但其他启用规则仍可命中相同文本。禁用不掩盖静态配置错误，仅由禁用规则或其 allowlist 依赖的变量缺失不报错；仍有启用规则依赖时照常校验。移除所有禁用声明并重新配置可恢复检测；在途请求保留旧快照，已有标记仍可还原。
- YAML 单引号正则保留反斜杠，多行标量的尾部换行符合声明；外部重复键、多文档、未知扩展名、非法文档和非字符串 regex/keywords 给出明确错误，不换解析器或静默转换类型。inline 必须是规则对象，不能把嵌套文本当成原生 YAML 规则。
- 至少包含 OpenAI、Responses、Anthropic、Gemini、Interactions 的消息、文本输出、工具调用/结果和可用流模式；包含旧 Completions、count_tokens/countTokens、compact、路由别名和可达的图片/视频/搜索文本入口。SDK native wrapper 与上游 ToFormat 不被混淆，回调以外的路径不标为已脱敏。
- 已知多模态节点中的文本可处理，图片/音视频载荷、签名、加密内容和协议标识保持原值；中文、多行、引号、反斜杠、数组以及对象/字符串形式工具参数在正常完成时保持可解析与语义正确。
- 同规则不同原文获得不同标记；同一作用域相同准确字节串在并发和后续请求中复用。不同身份、namespace、session 和独立请求不能相互还原。完整标记可精确替换；裸 ID、残缺/改写/未知标记不猜测；原文中的标记形文本不递归还原。
- session 缺失/null/空字符串才 fallback；非法类型、多值、控制字符、首尾空白和超长值报错。JSON Pointer 的数组和 ~0/~1 正确；选中来源后不因无映射换来源。同 ID 跨来源复用，不同 ID 不自动合并；缺少可信身份不建立会话。
- 普通无来源请求按 on_missing 处理；有效续接引用可找回同一会话，未知/过期/冲突引用不降级。规则/来源重配置、进程重启和二进制替换的映射行为有明确样例；活动 lease 不被容量/TTL 淘汰。
- 重发未变历史时，脱敏后的前缀保持一致，新增消息不重写旧标记；请求模式和映射失效按约定变化。此项验证本地输入稳定性，不把 mock 结果宣称为真实上游缓存命中。
- 每个完整标记枚举所有拆分位置，覆盖跨 chunk/SSE、JSON 转义、交错 choices/candidates/content blocks/tool calls。完整快照和终止聚合响应不重复文本；正常结束前排空字段尾部，结束之后不追加 delta。
- 模拟在标记前缀、ID、后缀中间断流：已输出内容保留，暂存候选尾部不发送，不调用 request.complete 补正文；每普通文本字段候选缓冲不超过 47 字节。SSE/工具解析缓冲另有上限；请求取消和无正常结束事件都能通过生命周期清理。
- 部分字段暂存不吞掉同块其他输出、usage 或控制事件；DropChunk 只用于整块无可交付内容。响应失败只降级一次，不回灌旧块、不猜测原文；找不到 RequestID 状态时保持 CPA 当前响应，不搜索其他会话映射。
- `(ITEM_A): (ITEM_A)` 配 secretGroup=2 能定位到第二项；ABCD 与 CDEF 同时匹配 ABCDEF 时覆盖并集。同范围规则优先级确定；gitleaks:allow 不绕过扫描，无效正则、无法定位和取消的部分扫描不伪装成未命中。
- 变量值中的正则字符、引号、换行、反斜杠、美元符和花括号按字面使用；数量词、Unicode 类和原捕获组编号不变。静态/动态规则与 allowlist 的行为跨请求不互相污染。
- 请求检测失败默认以 Terminate 阻止该次上游调用；显式请求错误放行在改写提交前回退并关闭当次还原。真实宿主 RPC 错误跳过行为与业务拒绝分别测试，不以宿主故障时仍保证拒绝作为验收条件。
- 规则缓存、请求/跳过状态、映射、续接索引、事件和工具 JSON 缓冲都有可测上限；完成回调重复或异步到达不重复释放。插件日志和配置序列化不暴露原文、标记、展开规则或身份/session 值。

计划完成的标准是：得到可装载于未修改 CPA 的 `cpa-vibeguard` 动态库及配置文档，主要 API 格式含 Gemini/Interactions 有可验证的文本与工具数据脱敏还原，session 和 header 跳过按配置工作，异常断流按约定丢弃缓冲，并给出其他 CPA 入口的实际覆盖边界。
