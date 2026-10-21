# 实测 API 覆盖

范围固定于未修改的 CPA v7.2.157（`09a29bd345bc44c473abe7fd07859e32df2ea543`）。测试装载真实 `cpa-vibeguard.so`，经 HTTP/WebSocket 调用 CPA，由本地 mock upstream 核对脱敏输入，再核对客户端的还原输出。表中的“已验证”描述这些路径和文本结构，不代表所有提供方、模型或后续 CPA 版本都已验证。

探针的 66 组实测入口保存在 [testdata/entrypoints.json](../testdata/entrypoints.json)：记录 CPA 实际的 SourceFormat、ToFormat、请求 key/结构、响应形态和有序钩子阶段，不记录正文、header 值或身份/session 值。CPA 的 metadata.request_path 有时是 Gin 模板，例如 `/v1beta/models/*action`；它不总是客户端原始 URL。

**对话、计数与工具。**

| 客户端入口 | 钩子 SourceFormat | 已验证路径与处理范围 |
| --- | --- | --- |
| `/v1/chat/completions` | `openai` | 普通 JSON、流式 choices、消息/工具调用与结果、字符串工具 arguments；OpenAI compatible 执行及 Chat→Gemini |
| `/v1/completions` | `openai` | 宿主在钩子前归一化为 Chat；普通与流式最终输出仍为旧 Completions 形态 |
| `/v1/responses` | `openai-response` | instructions/input、输出项、函数调用与结果、文本/工具 delta、done、完整快照；Codex 执行与 Responses→Chat |
| `/backend-api/codex/responses` | `openai-response` | 普通与流式别名，与 Responses 使用同一文本适配；上游 ToFormat 可为 `codex` |
| `/v1/responses/compact`、`/backend-api/codex/responses/compact` | `openai-response` | 普通 JSON、可解释的输入/输出文本；原生及 compatible compact；opaque compaction 内容保留 |
| `/v1/messages` | `claude` | 原生 Claude 与 Claude→Chat，system/content blocks、tool_use/tool_result、文本和参数增量 |
| `/v1/messages/count_tokens` | `claude` | 首方 HTTPS 计数路径（本地 CONNECT/TLS mock）、custom gateway 本地估算；先脱敏，计数响应保留数值 |
| `/v1beta/models/{model}:generateContent` | `gemini` | 原生 Gemini JSON；systemInstruction、contents/parts、functionCall.args、functionResponse.response、candidates |
| `/v1beta/models/{model}:streamGenerateContent` | `gemini` | 带/不带 `alt=sse` 的宿主流路径、逐 candidate finishReason；钩子中实际为 JSON records，保留 CPA 外层输出封装 |
| `/v1beta/models/{model}:countTokens` | `gemini` | 脱敏后的消息送往计数器，结果数值保留 |
| `/v1beta/interactions` | `interactions` | 原生及 Interactions→Gemini，input/steps/outputs、函数参数与结果、普通响应和事件流 |

请求按 SourceFormat 选择适配器，在 CPA 上游翻译之前脱敏。ToFormat 记录上游执行格式，不会触发第二次扫描或由插件再次做协议转换。响应使用该请求的输入格式和固定映射，还原钩子阶段可能早于客户端外层包装。

选定字段包含 Chat 的 `prediction.content`（字符串或 text 数组）、工具描述（含 Chat 的 `tools[].custom.description`、Responses 的平铺定义和 MCP `server_description`），以及 schema 中明确的 description/title。schema 遍历覆盖 `dependentSchemas`、schema 形式的 `dependencies`、`additionalItems`、`unevaluatedItems`、`unevaluatedProperties`、`propertyNames`、`contentSchema`；Gemini 函数声明的 `response` / `responseJsonSchema` 也按 schema 处理。Responses `text.format` 和 MCP 描述在请求、普通回显和流式完整快照中处理。模型名、工具名、自定义工具的 grammar 定义、JSON key、schema property/enum/required/约束、MCP 路由和认证字段、数字精度保留。对象工具参数与内嵌 JSON 字符串分别使用 codec，不对所有字符串递归替换。

普通/流式 fixture 覆盖中文、换行、引号、反斜杠、超过 2^53 的整数、并行输出和工具、opaque 数据。主要对话流在全部 49 个标记拆分位置验证；也覆盖工具参数逐字节增量、SSE 拆帧、usage、正常尾部和异常 EOF。Chat 自定义工具流也实测全部拆分位置，允许首块输入为空或只有类型信息，后续省略 `type` 时按 choice/tool 索引保留类型；冲突类型按响应错误降级，状态计入流内存限额。Responses/Codex 的 MCP 流允许 added 事件中的初始空参数，后续 delta 立即还原，done 与完成快照仍严格校验工具 JSON。计数依据脱敏内容，48 字节标记及可选提示会改变 token 数，不能当作原文计数。

**Responses WebSocket。** 两条路径 `/v1/responses` 和 `/backend-api/codex/responses` 都验证了同连接两轮文本/工具调用、连接隔离和正常残缺尾部；断连清理在 `/v1/responses` 上验证两种执行模式。覆盖原生 upstream WS 与配置 `websockets: false` 后的 HTTP execution；前者钩子收到裸 JSON，后者收到 SSE。插件使用显式的 execution_session_id 来源固定映射；握手本身不属于文本适配。固定宿主的跨连接 history 409 和两种上游模式的静默流断连延迟见 [OPERATIONS.md](OPERATIONS.md)。

**图片、视频和搜索。**

| 客户端入口/形态 | 钩子 SourceFormat | 实测行为与边界 |
| --- | --- | --- |
| `/v1/images/generations` | `openai-image` | compatible、xAI、Codex 图像执行；处理 prompt/negative_prompt 及可见 revised_prompt/text，图片字节/base64/URL 保留 |
| `/v1/images/edits` | `openai-image` | JSON 与 multipart 路径；只改 prompt/negative_prompt，文件内容与文件名不变；CPA 归一化为 JSON 后按实际 JSON 处理 |
| Responses 的图像执行 | `openai-response` | 可解释 input 文本和事件文本；不改 image_generation 结果载荷 |
| `/v1/videos`、`/v1/videos/generations`、`/v1/videos/edits`、`/v1/videos/extensions` | `openai-video` | xAI 生成/编辑/扩展的 prompt、可见文本响应；媒体字段保留 |
| `/openai/v1/videos` | `openai-video` | Sora compatible 入口的文本处理 |
| `/v1/videos/{request_id}` | `openai-video` | polling JSON 中的可见 prompt/text；只有显式复用同一 session 才能还原此前生成请求的标记 |
| `/openai/v1/videos/{video_id}/content` | `openai-video`（metadata poll） | 下载前先走有钩子的 JSON 元数据轮询；取得 URL 后的实际媒体传输保持原字节，媒体下载不做文本扫描 |
| `/v1/alpha/search`、`/backend-api/codex/alpha/search` | 无这些钩子 | mock 实际收到原查询；本插件不保护这两个 HTTP 路由 |

xAI 图像的“流式”结果由 CPA 在普通响应钩子之后合成，探针没有观察到相应的流块钩子，而且宿主合成结果不包含 revised_prompt；不能据此宣称逐块文本还原。compatible/Codex 图像生成的 SSE 路径则实测进入流式钩子。multipart 不虚构业务 JSON，JSON Pointer 的变量/session 来源不可由上传表单猜出，header/metadata 来源可用。

工具结果中的已知媒体节点也使用协议字段选择，包括 Responses/Codex 的 `function_call_output`、`custom_tool_call_output`、嵌套 MCP content，以及 Interactions 的 `function_result`。节点中的载荷、URL、文件名和类型保留，文本节点继续脱敏。任意工具参数中的 `type` 属性不会自行改变字段含义。

文本或工具字符串内的合法 `data:...;base64,...` URI 整段保留，URI 外的文本继续扫描。裸 base64 只有带可识别的图片或 PDF 文件头才跳过，支持 PNG、JPEG、GIF、WebP、BMP、TIFF、ICO、AVIF/HEIF 和 PDF；编码支持标准及 URL-safe 字母表、有无 padding，以及独立文件值的 CR/LF 折行。data URI 支持百分号转义。只校验编码和少量文件头，不解码扫描完整载荷，也不校验整张图片的有效性。普通 API key 不因符合 base64 字符集而被跳过；其他未标明类型的裸 base64 仍作为文本处理，传递这类文件时应使用协议媒体节点或带 MIME 类型的 data URI。

回归测试使用可正常解码、base64 中含手机号形状的 PNG，检查媒体字节不变且周围手机号仍被脱敏，另经真实宿主验证 Markdown 图片、工具参数和字符串形式工具结果的普通/流式往返。媒体仍计入请求、响应及流事件大小限额。

**SDK 格式、可解释文本与未覆盖内容。**

| 范围 | 证据与限制 |
| --- | --- |
| SDK `codex` | Responses 类字段适配，通过成对 fixture 与真实 C ABI 验证。上述 HTTP 路由的输入 SourceFormat 实际为 `openai-response` |
| SDK `antigravity` | Gemini 的外层 request/response 包装适配，通过单元与 C ABI fixture；不将所有 Antigravity 客户端入口标为实测 |
| SDK `codex-alpha-search` | query/queries 和结果 title/text/snippet 的 JSON 适配，通过单元与 C ABI fixture；不改变两个 HTTP 搜索路由绕过钩子的事实 |
| 多模态消息 | 已知节点中可解释文本按字段处理，图片/音视频二进制或 base64、签名、加密 thinking/compaction 保留；不做 OCR/转写 |
| Gemini cachedContent | 未暴露完整缓存创建与引用链；引用不作为 session ID，不承诺缓存内部文本或历史标记还原 |
| `/v1/models`、`/v1beta/models`、`/healthz` | 实测没有请求拦截钩子 |
| 管理与入站认证 | 未启用的 `/v0/management/config`、入站认证拒绝实测没有请求钩子；不扩展为所有管理操作的保证 |
| Realtime / Live 及其他未测路由 | 没有完整链路验收，不宣称受到保护；未经过钩子的请求不受 `on_unknown_api` 控制 |

已支持操作中的非法 JSON、重复成员名、混合互斥结构或未知消息节点按请求处理错误处置；不猜测成另一种协议，也不自动视为扫描完成。`on_unknown_api: passthrough` 只对插件能看见的未支持格式生效，不能拦住绕过钩子的路径。

**钩子与验证方式。** 普通执行的典型顺序为 `request.intercept_before → request.intercept_after → response.intercept_after → request.complete`。流式通常为 before → after → header 初始化通知 → 多个 stream_chunk → complete；header 通知仍使用 stream hook，探针记为 `response.stream_headers`。before 位于上游凭据选择之前，入站认证已经执行；凭据重试可有多次 after，但不重新脱敏。schema 6 的正文块不附带原请求与历史块，状态由 RequestID 关联。

入口记录由独立的 test-only C ABI probe 观察，生产库没有这些记录逻辑。普通测试把观测结果与 committed fixture 比较。维护者确需更新固定宿主覆盖证据时，可用下面命令生成记录后人工核对差异：

```sh
make integration
VIBEGUARD_RECORD_INVENTORY=1 go test -v -tags=integration -count=1 ./tests/integration/...
```

记录模式输出 `ENTRYPOINT {JSON}`，不会自动把未测试路径标为支持。相关测试见 [protocol_matrix_test.go](../tests/integration/protocol_matrix_test.go)、[media_test.go](../tests/integration/media_test.go)、[websocket_test.go](../tests/integration/websocket_test.go) 与 [probe_test.go](../tests/integration/probe_test.go)。原生协议样例见 [protocols.json](../testdata/protocols.json) 和 [streams.json](../testdata/streams.json)。宿主 RPC error 放行、成功 envelope 业务拒绝、配置更新、重启与替换的证据另记在 [IMPLEMENTATION.md](IMPLEMENTATION.md)。
