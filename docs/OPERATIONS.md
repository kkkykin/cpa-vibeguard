# 会话、运行与升级

本页基于未修改的 CPA v7.2.157、C ABI 1 / RPC schema 6。安装与版本表见 [README](../README.md)，所有配置键见 [CONFIGURATION.md](CONFIGURATION.md)。

启动时，`/healthz` 可先于客户端初始化完成响应；固定宿主随后首次加载 watcher 配置会重建执行器，此前建立的原生 WebSocket 可能收到 1012 `upstream requires HTTP replay`。部署就绪判断应同时确认本次启动的客户端加载、`file watcher started` 和插件注册日志，避免立即把模型流量送入仅已开始监听的进程。

**会话与缓存。** 默认每个 RequestID 独立持有原文映射，request.complete 后释放。显式配置 session 才跨请求复用；作用域是 `(namespace, caller_scope, session_id)`，来源类型和来源序号不参与作用域键。不同调用方、namespace 或 ID 不互相还原。session ID 的 header/JSON/metadata 选择只在第一次请求钩子执行；凭据重试不会换会话、生成新标记或重复追加提示。

同一作用域的相同 UTF-8 原文字节复用同一个随机标记，不做大小写或 Unicode 归一化。相同历史和规则下，后续请求的脱敏历史前缀保持稳定。请求模式、空闲回收、规则检测范围变化、重启或插件替换可能改变前缀，从而影响上游 prompt/KV 缓存。`prompt_cache_key` 不能抵消正文变化；本地测试只核对请求前缀一致，真实缓存命中与费用须由上游用量数据验证。

Responses 的 `previous_response_id` 和 Interactions 的 `previous_interaction_id` 通过已观察到的响应 ID 找回已认证会话。显式 session 与引用必须指向同一作用域；无来源但引用有效时可沿用。引用未知、过期、冲突或无法唯一定位时按请求错误处理，默认返回 `session_resolution_failed`，不会创建新会话继续猜测。未配置 session 时依赖历史引用也会失败。选择 `on_request_error: passthrough` 会显式关闭该次请求保护，包括这类错误，见后文。

映射仅存在于当前插件实例内存。空闲 TTL/LRU 只回收无活动 lease 的作用域，长时间运行的流不会仅因 TTL 到期被强行淘汰。达到容量且无法回收时，新请求失败；活动请求继续使用自己的快照。兼容的配置更新保留映射，包括禁用规则、重新启用和切换到返回同一 ID 的来源。旧 ID 索引带映射代次，不能错误绑定到同名新会话。

重启、成功替换动态库、会话过期后，客户端应重新发送构建上下文所需的原文，并建立新会话；旧标记本身不能恢复原文，旧续接引用默认拒绝。外部缓存脱敏响应时，必须同时约束调用方、会话和映射生命周期。Gemini cachedContent 等服务端缓存未暴露完整创建链，不能当作自动 session source 或已登记历史使用。

<a id="execution-session-metadata"></a>

**CPA metadata 的来源。** `metadata: /execution_session_id` 读取 CPA 组装的请求执行上下文。宿主通过 `RequestInterceptRequest.Metadata` 将它传入插件；请求正文 `Body`、请求头 `Headers` 和执行上下文 `Metadata` 是独立字段。客户端正文中的 `metadata` 不会整体合并到这个执行上下文。

固定版本 CPA 在请求执行前收集 HTTP 请求信息、入站认证结果和内部 Context，并补充模型执行信息：

| metadata 字段 | 来源与可用条件 |
| --- | --- |
| `request_path` | 当前 HTTP 请求的 Gin 路由模板；没有模板时使用 URL path，非 HTTP 执行可能缺失 |
| `caller_scope` | 入站认证成功后的 Principal，经 `CallerScope` 派生为 SHA-256 标识；缺少认证身份时不写入 |
| `execution_session_id` | 内部 Context 中已设置的执行会话 ID；未设置时不写入 |
| `idempotency_key` | 客户端显式提供的 `Idempotency-Key` header，经去除首尾空白后非空才写入 |
| `requested_model` | 执行入口补充的客户端请求模型名 |

这些字段的来源各不相同：例如 `caller_scope` 由入站认证身份派生，而 `idempotency_key` 来自客户端 header。`metadata` 表示宿主提供的数据对象，不表示其中每个字段都是认证身份。

假设客户端请求正文包含：

```json
{
  "model": "some-model",
  "metadata": {"session_id": "chat-123"},
  "input": "你好"
}
```

`json: /metadata/session_id` 会从上述正文读到 `chat-123`；`metadata: /execution_session_id` 则从独立的宿主执行上下文取值。在正文中另外添加 `metadata.execution_session_id`，也不会设置或覆盖宿主的同名字段。若要读取那个正文值，应显式配置 `json: /metadata/execution_session_id`。

**`execution_session_id` 的生成与传递。** 在固定版本的标准入口中，Responses WebSocket 采用以下过程：

1. WebSocket upgrade 成功后，[ResponsesWebsocket](https://github.com/router-for-me/CLIProxyAPI/blob/09a29bd345bc44c473abe7fd07859e32df2ea543/sdk/api/handlers/openai/openai_responses_websocket.go#L267) 调用 `uuid.NewString()`，为这条下游连接生成 `passthroughSessionID`。生成发生在读取多轮消息的循环之外。
2. 每轮模型执行前，调用 `handlers.WithExecutionSessionID(cliCtx, passthroughSessionID)`，把同一个 ID 放入本轮的内部 Context。
3. [requestExecutionMetadata](https://github.com/router-for-me/CLIProxyAPI/blob/09a29bd345bc44c473abe7fd07859e32df2ea543/sdk/api/handlers/handlers.go#L196) 从 Context 读取非空 ID，写入 `meta["execution_session_id"]`，随后用于执行选项的 `opts.Metadata`。
4. [请求钩子构造](https://github.com/router-for-me/CLIProxyAPI/blob/09a29bd345bc44c473abe7fd07859e32df2ea543/sdk/api/handlers/handlers_interceptors.go#L467) 将 `opts.Metadata` 交给插件。配置并选中该来源时，VibeGuard 在第一次请求钩子中按 `/execution_session_id` 读取它并固定会话选择。

其稳定范围取决于客户端与 CPA 之间的连接：

| 请求场景 | `execution_session_id` 的行为 |
| --- | --- |
| 同一条 Responses WebSocket 连接的多轮请求 | 复用同一个 UUID；每轮仍有独立的 RequestID |
| 断开后重新建立 Responses WebSocket 连接 | 生成新 UUID；若选用此来源，将选择新的插件会话作用域 |
| 普通 HTTP 请求，包括 SSE 流式响应 | 标准入口不会自行生成该字段；`stream: true` 或 HTTP 连接复用不会使它出现 |

同一条下游 Responses WebSocket 无论最终使用原生 upstream WS，还是 `websockets: false` 的 HTTP execution，都沿用上述 ID。CPA 其他从 header/body 提取或推导会话身份的逻辑，不会自动把这些身份写成 `execution_session_id`。自行嵌入 CPA SDK 的宿主代码可以调用 `WithExecutionSessionID` 设置 Context；普通 HTTP 客户端不能通过正文直接调用这条内部传递路径。

例如，在一个 session profile 中配置以下来源顺序：

```yaml
sources:
  - header: X-Session-ID
  - json: /metadata/session_id
  - metadata: /execution_session_id
```

前两项让客户端显式指定会话，可用于 HTTP 或 WebSocket；最后一项让没有显式 ID 的 Responses WebSocket 请求按连接复用映射。若前两项已选出有效 ID，便不会再使用连接 UUID。普通 HTTP 请求若前两项都缺失，且没有其他宿主代码设置执行会话 ID，最后一项也缺失；无有效续接引用时，按 `on_missing: request|reject` 使用独立请求或拒绝。非法来源值仍按错误处理，不会继续 fallback。完整 profile 与来源选择规则见 [CONFIGURATION.md](CONFIGURATION.md)。

**正常结束与异常断流。** 普通响应只精确替换本作用域中完整的 48 字节标记，单次替换后不再扫描插入的原文。未知、改写、残缺标记原样保留。流式按输出/候选、内容块、工具调用和字段分别处理：完整标记收齐即还原，普通文本只保留可能组成标记的末尾，最多 47 字节。

| 正常事件 | 行为 |
| --- | --- |
| Chat 每个 choice 的 `finish_reason` | 输出该 choice 的文本尾部，再结束 |
| Claude `content_block_stop` | 关闭对应内容块之前输出尾部 |
| Responses 文本/参数 done 与 completed | 在对应 done 前补出必要 delta，独立还原完整快照；保留事件顺序和整数序号 |
| Gemini 每个 candidate 的 `finishReason` | 排空该 candidate，保留并行候选 |
| Interactions 内容与交互终止事件 | 排空相应字段，保留控制/签名等不透明数据 |

完整快照独立还原，不与先前 delta 再拼成重复内容。正常结束时，无法组成完整标记的文本尾部按字面输出；未完成或非法的工具 JSON 按还原错误处理。角色、usage、其他输出和控制事件不会因一个字段暂扣尾部而整体丢失。

上游 EOF、错误或客户端取消没有正常结束事件时，丢弃尚未输出的候选尾部、未完成事件和工具解析缓冲，保留已经交付的内容。`request.complete` 只做幂等清理与释放 lease，不返回正文；宿主外层合成的 `[DONE]` 不保证再次经过插件。47 字节只描述普通文本的标记尾部，SSE、JSON 转义和工具参数另有缓冲与限额。

Responses WebSocket 已验证原生 upstream WS 和显式 `websockets: false` 的 HTTP execution 两种路径。每轮仍走 Responses 执行钩子；要复用插件映射，须显式配置 `metadata: /execution_session_id` 或稳定 header/JSON session 来源。固定宿主会在插件之前以 409 `previous_response_not_found` 拒绝跨连接历史引用。两种上游模式都存在静默流边界：客户端 WS 断开后，宿主可能等到下一次写入、keepalive 或上游完成才观察到断连；这段期间活动 lease 仍保留。测试先观察 429，再由上游发送进度事件，验证取消与清理，不依赖 TTL 淘汰活动请求。未承诺任意 upstream WS upgrade 错误都能透明回退 HTTP。

**请求与响应故障。**

| 情况 | 插件行为 |
| --- | --- |
| `on_unknown_api: reject` | 未注册/缺失输入格式返回 HTTP 400、`unsupported_api` |
| `on_request_error: reject` | 已知请求的处理错误返回 HTTP 400，成功 RPC envelope 内设置 Terminate，阻止本次上游调用 |
| 活动请求数达到上限 | HTTP 429、`inflight_limit` |
| 实例 quiesce/关闭等不可用 | 在钩子仍被调用的前提下，HTTP 503、`plugin_unavailable` |
| `on_unknown_api: passthrough` | 未支持格式整次放行，无原文映射和响应还原 |
| `on_request_error: passthrough` | 改写提交前回到本次钩子的输入正文，撤销临时映射并整次跳过；不删除其他请求已发布的映射 |
| 普通响应还原失败或超限 | 整个响应保持 CPA 当前正文，记录一次降级；不会返回部分还原结果 |
| 流事件解析/还原失败或超限 | 丢弃以前尚未输出的缓冲，停止该次响应的后续还原；出错的当前块及后续块交由 CPA 原流程处理 |
| 响应没有 RequestID 状态 | 保持 CPA 当前正文，不跨会话搜索映射 |
| 插件未加载、RPC 错误、熔断 | CPA 可能跳过钩子继续请求，原文可能到达上游 |

业务拒绝正文形如 `{"error":{"type":"vibeguard_request_rejected","code":"invalid_request_body"}}`，不包含原文、工具参数、动态规则或身份/session 值。响应降级不重放旧块，也不伪造正常结束；畸形或截断流不保证完整工具 JSON。返回 RPC error 不能让固定宿主强制终止流，因此插件区分业务 Terminate 与宿主故障边界。

内部计数区分 header 跳过、未知格式放行、请求错误放行、拒绝、保护和响应降级；每个响应只计一次降级。目前这些是 Go 核心的计数接口，用于测试，没有新增 HTTP metrics/管理端点。插件关闭 Gitleaks 的内容诊断日志，仅在热配置拒绝时写无值诊断。CPA 自身仍可能在钩子前记录请求，宿主日志策略需独立配置。

**热配置更新。** 候选配置先完整解析、合并规则、校验并读取所需环境，再原子发布。新请求使用新快照，在途请求保留旧规则、session 选择、限额和映射。外部规则文件没有独立 watcher，修改后需让 CPA 重新配置插件或重启。

初次注册配置无效时返回 RPC error。已有工作配置的 `plugin.reconfigure` 失败时，插件保留旧配置并返回含原有四项能力的成功 registration；附带 `configuration_error` 字段，同时向 stderr 输出：

```text
cpa-vibeguard: reconfigure_rejected; previous configuration retained: "<安全错误码及来源位置>"
```

这样处理是因为固定 CPA 会在 reconfigure RPC error 后移除插件钩子。CPA 不显示 registration 内这个扩展字段；运维应查看上述 stderr 诊断并验证新规则的实际行为。**CPA 外层的 “config successfully reloaded” 不代表插件候选配置已生效。** 错误只报告安全错误码、字段位置、来源路径或 rule ID，修复后重新配置即可。

固定宿主的配置 watcher 监视配置文件 inode。集成测试在原路径原 inode 内写入完整新内容来触发更新；原子 rename 替换配置文件没有触发预期重载。部署工具若通过 rename 更新配置，应使用已验证的宿主重载方式或重启，不能仅看文件内容判断生效。该限制只描述当前宿主版本。

**动态库升级与重启。** 配置热更新可保留兼容映射，二进制替换会创建新实例，不迁移内存映射。先停止接入新的模型请求并排空在途请求，安排重建需要续接的会话。quiesce 用于生命周期协调，不是会话迁移协议。

1. 在独立构建目录完成 `make check` / `make integration`，保留当前库的回滚副本到插件扫描目录外。
2. 使用新版本化路径，例如从 `cpa-vibeguard-v0.1.0.so` 升到 `cpa-vibeguard-v0.1.1.so`。编号仅演示宿主命名约定，不表示已有这些发布版本；插件 ID 和配置键仍为 `cpa-vibeguard`。
3. 先完整写入新路径的 `.next` 文件，再在同一文件系统 rename 为最终 `.so`。不要覆盖正在被进程 mmap 的旧库，也不要在运行中的 `dist` 上直接重新构建。
4. 触发一次实际的 CPA 重新配置或重启。实测热替换通过在原配置文件中改变插件 priority 触发，并等待 `plugin hot reloaded`；priority 会影响与其他插件的顺序，需保持部署所需顺序。
5. 检查新实例的注册/加载日志，验证新的脱敏往返。旧续接 ID 默认应报错；重建上下文后会获得新标记。未知的旧标记保持原样。

在没有宿主版本固定策略干预时，CPA 会选择同 ID 的版本化候选；只覆盖同一路径，即使文件内容变了，也不会让已加载实例自动切换。测试用新路径和原子 rename 验证成功替换，并用无效动态库验证 quiesce/reconfigure 回滚恢复旧实例及其映射。失败回滚不意味着新二进制已经生效；检查日志和行为后再恢复流量。

**通过 Caddy 按路径跳过。** 插件只判断 header；前置代理负责路径选择和剥离。将以下片段与插件的 `X-Vibeguard-Skip: "true"` 精确条件配套使用：

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

例如 `/unmasked/v1/chat/completions` 转成 CPA 的 `/v1/chat/completions` 并跳过插件。正常路径删除客户端自带的跳过头，跳过路径则由代理覆盖注入；所有路径仍需 CPA 认证。`/unmasked/*` 本身允许调用方主动选择原文路径，应只在部署确实需要它时开放。若依赖代理约束跳过头，应限制客户端直接访问 CPA 监听地址。

示例文件 [examples/Caddyfile](../examples/Caddyfile) 已通过 Caddy v2.10.2 与真实 CPA 的 JSON/SSE、路径剥离、header 边界、原样标记及认证测试。启动可用 `bin/caddy run --config examples/Caddyfile --adapter caddyfile`；无需自定义 Caddy 模块。
