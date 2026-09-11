本次评估基于 `/tmp/CLIProxyAPI` 的提交 `09a29bd345bc44c473abe7fd07859e32df2ea543`，核对日期为 2026-09-11。本文保留选型依据与已接受的宿主边界；正式实施约定见已更新的 [PLAN.md](/data/workspace/cpa-vibeguard/PLAN.md)。

**选型已确定：实现 CPA 原生插件 cpa-vibeguard，不修改宿主。** 用户已接受异常断流时丢弃暂存的残缺标记，以及宿主现有错误处理、日志和路由行为。原先针对 Caddy 模块提出的全部 HTTP 透明转发及端到端 fail-closed 保证不再作为当前插件的验收条件。

完整标记收齐后立即还原；正常协议结束事件到达时处理残缺尾部；没有结束事件的 EOF 则丢弃尚未输出的缓冲并释放请求状态。协议范围扩展到 Gemini、Interactions 及 CPA 其他可达的文本执行入口，首版仅提供 header 跳过条件；路径判断由前置 Caddy 反代注入该头完成。具体配置、覆盖矩阵与实施顺序以 PLAN.md 为准。

宿主已经负责模型路由、凭据选择、协议转换、上游重试和响应发送，可以省去 VibeGuard 对 Caddy ResponseWriter、Flush、ReadFrom 等接口的包装。Gitleaks 精确定位、稳定标记、会话隔离、工具参数解码和跨增量还原仍须实现，换成 plugin 不会消除这些核心工作。

| 需求 | 当前源码提供的能力 | 对 VibeGuard 的影响 |
| --- | --- | --- |
| 请求脱敏 | 请求钩子包含 RequestID、协议、请求头、正文及 metadata，可替换正文或主动终止 | 接入直接；在请求钩子保存原文映射 |
| 普通响应还原 | 响应钩子可按 RequestID 找到映射并替换正文 | 覆盖成功响应；执行错误另走错误路径 |
| SSE 增量还原 | 流钩子可替换或 DropChunk，具有 RequestID 和 ChunkIndex | 可以暂存被拆开的标记；仍需各协议的字段、工具参数和事件处理 |
| 多来源 session fallback | 可读取请求头、正文和 caller_scope；宿主也有内置会话提取 | 可以按计划实现，但不能直接沿用宿主固定优先级及宽松校验 |
| 内部协议适配 | SourceFormat 已由宿主处理器确定 | 直接查找字段/流式适配器；不提供 api 配置或跨协议正文探测 |
| 未匹配时可选直通 | 插件可以不改写当前请求 | 后续仍由 CLIProxyAPI 路由、校验和转换，不保证原始 HTTP 字节透明转发 |
| 配置与发布 | YAML 插件配置、动态加载、重配置和 quiesce 已有机制 | 可以复用；Go 插件使用 C ABI 动态库，需要 CGO 和对应平台产物 |

**已接受的宿主边界：**

1. **插件错误默认跳过。** [请求拦截器调用](/tmp/CLIProxyAPI/internal/pluginhost/adapters_interceptors.go:18) 遇到错误只记录警告；宿主捕获到的 panic 会使插件熔断。[调用链](/tmp/CLIProxyAPI/internal/pluginhost/adapters_interceptors.go:109) 保留此前的正文继续执行，因此 VibeGuard 尚未成功脱敏时，原文可能继续发往上游。响应和流钩子也有类似行为。

   插件能把已处理的检测、校验等失败转换为成功 RPC 返回中的 `Terminate: true`，这会阻止上游调用，见 [RequestInterceptResponse](/tmp/CLIProxyAPI/sdk/pluginapi/types.go:1062)。但普通 `return err` 不会拒绝请求，RPC 故障及被跳过的插件也无法靠这段业务逻辑兜底。当前方案默认对可处理的请求错误主动 Terminate，接受宿主 RPC 故障放行；不要求新增必需插件策略。`on_unknown_api: passthrough` 和 header bypass 是另外两种显式放行，不能与故障混为一谈。

2. **流结束回调不能补发尾部，也没有直接终止流的返回值。** [StreamChunkInterceptResponse](/tmp/CLIProxyAPI/sdk/pluginapi/types.go:1169) 只有正文、头部和 DropChunk 等字段；空正文表示保留原块，暂扣块必须使用 DropChunk。[流读取循环](/tmp/CLIProxyAPI/sdk/api/handlers/handlers_stream.go:677) 在 EOF、取消或上游错误时直接退出，没有同步的最终流钩子。[request.complete](/tmp/CLIProxyAPI/internal/pluginhost/adapters_interceptors.go:189) 异步执行，适合释放映射和缓冲，无法向已结束的响应追加正文。

   正常协议结束事件本身会经过流钩子，可以在转发相应结束事件之前排空该字段的缓冲：Chat Completions 使用每个 choice 的非空 finish_reason，Anthropic 使用 content_block_stop，Responses 使用 output_text.done、function_call_arguments.done 等字段结束事件，并处理相应的请求终止事件。已经结束的字段不能等到整个 response.completed 或 message_stop 后再追加 delta。协议适配器负责生成合法数据和保持事件顺序；这不要求额外的 EOF 回调。

   具体缺口是上游未发出这些事件就 EOF、断开或报错时，插件没有最后一次返回正文的机会。比如已经输出“地址是 ”，暂存了 `[[redacted:v1:01234567`，后续内容却没有到达；只能清理这个尾部，不能再把它原样补给客户端。即使有 EOF 回调，半个标记也不能可靠还原成原文，能补发的只是残缺文本。客户端主动断开时本来也无法继续接收。Chat 的 `[DONE]` 可能由[外层发送代码](/tmp/CLIProxyAPI/sdk/api/handlers/openai/openai_handlers.go:686) 在结束后生成，不能把收到该标记当作插件一定有最后一次回调的保证。

   当前方案仅缓存可能构成标记的必要尾缀，完整标记立即替换，确定不是标记的文字立即输出。对已解码的普通文本字段，当前固定标记长 48 个 ASCII 字节，候选尾缀最多 47 字节；这是每字段的标记缓冲上限，不包括协议解析或工具 JSON 缓冲。工具参数采用增量处理和独立上限，不能缓存整个参数后仍声称异常只丢几十字节。异常结束时丢弃未输出缓冲，已输出结果保留；同步 EOF 补发接口和 executor 包装均不列为实施前置条件。

3. **宿主日志先于脱敏。** [请求日志中间件安装位置](/tmp/CLIProxyAPI/internal/api/server.go:144) 在认证和模型执行之前，[中间件](/tmp/CLIProxyAPI/internal/api/middleware/request_logging.go:27) 会捕获原始请求正文。关闭普通请求日志仍存在错误日志和大正文临时落盘路径，见 [forceLog](/tmp/CLIProxyAPI/internal/api/middleware/response_writer.go:289)。当前计划约束插件自身日志不含秘密，接受宿主独立的日志行为。源码中的 `commercial-mode` 会跳过该请求日志中间件，但不能将其等同于对所有日志路径的完整审计。

4. **插件位于模型执行链。** [路由注册](/tmp/CLIProxyAPI/internal/api/server_routes.go:61) 先把不同 URL 分配给协议处理器，拦截器随后参与模型执行。不改写插件正文，只代表交还宿主处理。当前直接按 CPA SourceFormat 分发内部适配器，再校验其具体结构；不需要 api 配置或自行猜测协议。不创建任意未知协议的反代路由，也不承诺原始 HTTP 压缩字节透明传输。header bypass 在首次请求钩子固定并贯穿响应，前置 Caddy 可按路径注入这个头。

**采用的接入方式：**

cpa-vibeguard 使用纯 Go 核心，由薄适配层接入 `request_interceptor`、`response_interceptor`、`response_stream_interceptor` 和 `request_lifecycle_plugin`，能力名称见 [RPC 注册结构](/tmp/CLIProxyAPI/internal/pluginhost/rpc_schema.go:35)。从 [request-lifecycle 示例](/tmp/CLIProxyAPI/examples/plugin/request-lifecycle/README.md:1) 起步，不把有状态的脱敏还原放入 translator/normalizer：其请求结构缺少 RequestID、请求头和 metadata，见 [RequestTransformRequest](/tmp/CLIProxyAPI/sdk/pluginapi/types.go:1006)。

规则输入采用有序 rule_sources：inline 直接使用 YAML 规则对象，外部文件支持 Gitleaks TOML 和同结构 YAML，统一成一份内部规则表示后交给相同检测流程。插件配置 disabled_rules 与规则文档的 extend.disabledRules 合并后按 ID 禁用内置或自定义规则。协议差异仅留在字段/流式适配层，检测器与规则格式无须感知具体 API。

在 `request.intercept_before` 固定会话来源和映射作用域，再脱敏；这里的 before-auth 指上游凭据选择之前，入站认证已经执行。可使用经入站认证形成的 [caller_scope](/tmp/CLIProxyAPI/sdk/api/handlers/handlers.go:290)，可信身份缺失时仍须遵守计划的拒绝规则。宿主的 [会话提取顺序](/tmp/CLIProxyAPI/sdk/cliproxy/session/info.go:34) 是固定的，部分校验还会去空格或把无效值当作缺失；计划要求的有序 sources、严格错误和 namespace 隔离应由核心 SessionResolver 承担。

宿主会把改写后的正文同时写入 [Payload 和 OriginalRequest](/tmp/CLIProxyAPI/sdk/api/handlers/handlers_interceptors.go:484)，因此映射必须在请求阶段保存，响应阶段不能指望 OriginalRequest 仍有原文。同一会话复用 `[[redacted:v1:<id>]]` 的缓存策略、完整标记才还原的约定，都与宿主选择无关。

以当前源码的 [ABI 1 / schema 6](/tmp/CLIProxyAPI/sdk/pluginabi/types.go:5) 为兼容起点。schema 3 起省去普通流块中的请求正文，schema 5 起省去历史块；插件应维护按 RequestID 隔离的有界状态。宿主提供 quiesce，但活动请求的映射排空、会话映射在重载时的连续性及 TTL 仍需设计和验证。

**验证范围：**

选型调研阶段已运行现有 `internal/pluginhost` 与 `sdk/api/handlers` 两个包中关于错误跳过、panic 熔断、主动拒绝、请求和响应改写、流丢块、请求完成及 quiesce 的定向测试，均通过。关键行为有现成测试覆盖：[TestInterceptorsSkipErrorsAndFusePanics](/tmp/CLIProxyAPI/internal/pluginhost/adapters_test.go:1551)、[TestHandlerStreamInterceptorRewritesAndDropsChunks](/tmp/CLIProxyAPI/sdk/api/handlers/handlers_interceptors_test.go:971)。后者在三个数据块后关闭流，只收到一次头部初始化和三次块回调。

这些验证针对宿主接口和控制流；尚未实现或验证 cpa-vibeguard 动态库的端到端脱敏、性能或真实上游缓存命中。
