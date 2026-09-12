# 配置与规则

以下片段均放在 CPA 的 `plugins.configs.cpa-vibeguard` 下；完整宿主示例见 [examples/cpa.yaml](../examples/cpa.yaml)。`plugins.enabled`、`plugins.dir`、单插件 `enabled` / `priority` 和商店安装后写入的 `store` 元数据由 CPA 管理。插件在注册和热配置时忽略顶层 `store`，保留宿主对安装来源和版本的管理。插件不提供 `api` 选择、路径前缀、`fail_open` 或远程规则订阅配置。

**默认行为。** 除宿主的 enabled/priority/store 外，省略配置相当于：

```yaml
on_unknown_api: reject
on_request_error: reject
preserve_marker_hint: false
bypass:
  headers: []
variables: {}
disabled_rules: []
rule_sources: []
limits:
  request_bytes: 8388608
  response_bytes: 8388608
  event_bytes: 1048576
  tool_json_bytes: 1048576
  request_state_bytes: 16777216
  max_inflight: 256
# 不声明 session：每个请求独立持有映射。
```

默认扫描使用 Gitleaks 官方密钥规则；邮箱、手机号、身份证、银行卡和 IP 地址需要额外规则。规则零命中时文本保留原样，属于正常扫描结果，不会触发 `on_request_error: reject`。`preserve_marker_hint: false` 只关闭标记保留提示，不关闭扫描。

官方规则有格式、长度、熵和 allowlist 约束。例如 `sk-proj-abcdefghijklmnopqrstuvwxyz123456` 不符合官方 OpenAI 密钥格式，其中的 `abcdefghijklmnopqrstuvwxyz` 还在官方全局 stopwords 中；添加宽松正则也不能覆盖这个全局忽略条件。这样的占位符原样保留，不能单独用来判断插件是否已启用。

| 配置 | 含义 |
| --- | --- |
| `on_unknown_api: reject\|passthrough` | 对缺失/未注册的 CPA SourceFormat 拒绝或整次放行；不把非法正文和未知消息节点当成未知 API |
| `on_request_error: reject\|passthrough` | 对已接管请求的解析、规则、变量、session、映射等错误拒绝或在改写提交前整次放行 |
| `preserve_marker_hint` | 在协议允许的指令位置追加一次固定的完整标记保留提示，默认 false；不会覆盖原有指令 |
| `bypass.headers` | 有序 header 条件，任一命中即整次跳过，默认空 |
| `variables` | 显式声明 `{var.NAME}` 的 env/header/JSON/metadata 来源 |
| `disabled_rules` | 精确匹配、区分大小写的内置或自定义 rule ID 列表 |
| `rule_sources` | 按顺序合并的绝对文件路径或 inline mapping |
| `session` | 可选的跨请求映射配置，见下文 |

插件自身配置严格校验类型、未知字段、null、重复键和 JSON Pointer；宿主的 `store` 内容不参与插件配置校验。直接读取的 YAML 只允许单文档，错误不会转用其他解析器重试。CPA 传入的 inline 按宿主交付的 YAML 结构校验；宿主解析之前已丢失的信息无法由插件恢复。初次无效配置注册失败；无效热配置保留原工作快照并输出诊断，详见[运维文档](OPERATIONS.md)。

**规则来源与合并。** 默认加载 Gitleaks v8.30.1 的官方规则。外部 `.toml` 使用 Gitleaks TOML，`.yaml` / `.yml` 使用同结构 YAML，扩展名不区分大小写。`file` 必须是 CPA 进程可读的绝对路径。下面同时加载仓库的两个样例，并在末尾追加 inline 规则：

```yaml
rule_sources:
  - file: /opt/cpa/rules/team.toml
  - file: /opt/cpa/rules/custom.yaml
  - inline:
      rules:
        - id: internal-account
          description: Internal account identifier
          regex: '\bACCT-[A-Z0-9]{8}\b'
          keywords: ['ACCT-']
```

将 [team.toml](../examples/rules/team.toml) 和 [custom.yaml](../examples/rules/custom.yaml) 安装到配置所写的实际位置。每项恰好声明 `file` 或 `inline`；inline 必须直接是对象，不接受 `inline: |` 中再嵌套文档文本。把整个 inline 对象移入 YAML 文件，语义相同。

所有来源按声明顺序合并。后面的相同 ID 只覆盖显式提供的字段，缺省字段继承；显式 `0`、`false`、空数组与缺省不同。allowlists 追加，支持仅为已有 ID 追加 allowlist；给不存在的 ID 只写 allowlist 会报错。同一文档重复 ID 报错。`extend.useDefault` 取最后一次显式声明，默认 true；设为 false 可只用自定义规则。

YAML 保留 Gitleaks 的字段名和大小写，不另设 snake_case 别名：

| 位置 | 支持字段 |
| --- | --- |
| 规则文档 | `title`、`description`、`minVersion`、`extend`、`rules`、`allowlists`（或旧式 `allowlist`） |
| `extend` | `useDefault`、`disabledRules` |
| 每条规则 | `id`、`description`、`regex`、`secretGroup`、`entropy`、`keywords`、`tags`、`allowlists`（或 `allowlist`） |
| allowlist | `description`、`condition`、`regexTarget`、`regexes`、`stopwords`；全局 allowlist 另可写 `targetRules` |

兼容范围是 v8 文本检测规则。仓库路径、commit、递归 extend、required、skipReport 等未覆盖字段会报错。`minVersion` 不能要求高于固定 Gitleaks 版本。旧式单数 allowlist 会归一化；同层同时声明非空单数/复数内容会报错。插件提供 YAML 表示，不要求 Gitleaks CLI 支持 YAML。

`secretGroup` 指定需要替换的捕获组，未指定时沿用 Gitleaks 的选择语义。keywords 是命中预筛选，不是独立的“全部替换字符串”。插件按 UTF-8 字节精确定位，相交 span 取并集；请求中的 `gitleaks:allow` 注释不能跳过检测，也不额外解码 base64/URL 内容。

正则建议使用 YAML 单引号，以保留 `\b`、`\d` 和反斜杠；多行可用 `|-` 去掉末尾换行。数字、布尔值和 null 不会转换为 regex/keywords 字符串。可选 PII 包 [pii.yaml](../examples/rules/pii.yaml) 包含邮箱和中国大陆手机号规则，只有加入 `rule_sources` 后启用；它不是完整 PII 识别器。

需要同时处理身份证、银行卡和 IPv4 时，可改用 [privacy-text.yaml](../examples/rules/privacy-text.yaml)，它已包含邮箱和手机号规则：

```yaml
rule_sources:
  - file: /opt/cpa/rules/privacy-text.yaml
```

先将文件安装到配置中的绝对路径，再让 CPA 重新配置插件或重启。这些附加规则按文本格式匹配：身份证和银行卡不做校验位验证，银行卡规则也会命中其他 13–19 位数字。它们不等同于旧 Caddy 插件带校验和过滤逻辑的完整内置规则。

**禁用与 allowlist。** 例如：

```yaml
disabled_rules: [github-pat]
rule_sources:
  - file: /opt/cpa/rules/custom.yaml
  - inline:
      rules:
        - id: internal-hostname
          allowlists:
            - description: Public examples
              regexTarget: secret
              regexes: ['^(docs|example)\.internal$']
```

顶层 `disabled_rules` 与所有来源的 `extend.disabledRules` 取并集，去重后在合并与静态校验之后、变量绑定之前移除规则。后面的同 ID 定义或空禁用列表不会重新启用它；须从所有禁用位置移除并重新配置。未知/空/非字符串 ID 报错，不支持通配符。禁用一个规则后，同一文本仍可能命中其他规则。

禁用不豁免文件解析、字段类型、模板语法和静态正则错误。只有最终启用规则及适用 allowlist 的变量才要求提供；仅供禁用规则使用的变量可缺失。全局 `targetRules` 在禁用前检查引用存在性，指向已禁用 ID 不算错误。规则变化不会删除已发布的原文映射。

allowlist 可按 `regexes` 或 `stopwords` 过滤命中；`condition` 默认 OR，也支持 AND（及 `||` / `&&`）。`regexTarget` 可选 `secret`、`match` 或 `line`，默认 secret。全局 allowlist 的 `targetRules` 限定适用 ID，省略则应用于全部启用规则；规则级无需也不能声明 targetRules。

**环境与请求变量。** `{env.NAME}` 直接读取 CPA 进程环境；`{var.NAME}` 必须先声明来源。变量名称匹配 `[A-Za-z_][A-Za-z0-9_]*`，每项恰好选 env/header/json/metadata 之一：

```yaml
variables:
  tenant: {header: X-Tenant}
  project: {json: /metadata/project}
  execution: {metadata: /execution_session_id}
  region: {env: VIBEGUARD_REGION}
rule_sources:
  - inline:
      rules:
        - id: tenant-value
          regex: 'tenant=({var.tenant})'
          secretGroup: 1
          keywords: ['tenant=']
        - id: company-email
          regex: '\b[A-Za-z0-9._%+-]+@{env.VIBEGUARD_DOMAIN}\b'
          keywords: ['@']
```

此例实际依赖 `X-Tenant` 和环境 `VIBEGUARD_DOMAIN`；其余声明未被规则引用，不要求值存在。环境在配置快照构建时读取；请求值取第一次钩子的 header、脱敏前 JSON 和 metadata，固定至请求结束。JSON 与 metadata 使用 RFC 6901 Pointer，例如 `/metadata/session_id`、`/items/0/name`，`~0` 代表 `~`，`~1` 代表 `/`，不是 JSONPath。

`json` 以业务请求正文为根，`metadata` 以 CPA 单独传给插件的执行上下文为根。例如，正文中的 `metadata.session_id` 应通过 `json: /metadata/session_id` 读取；它不会自动写入宿主 metadata。宿主字段的来源与可用条件见 [CPA metadata 说明](OPERATIONS.md#execution-session-metadata)。

模板仅在启用规则的 regex/keywords 和适用 allowlist 的 regexes/stopwords 中展开。regex 内按字面量转义并用非捕获组包裹，keywords/stopwords 内使用原始值；不会改变原有捕获组编号，也不会把变量当作 YAML/TOML 片段或正则代码。数字量词、Unicode 类、转义花括号继续按正则解释；字符类内或未完成转义处插值报错。规则 ID、路径、数值和描述不做模板展开。

实际依赖的变量缺失、空值、非字符串、header 多值或超限均报错。multipart 请求没有可供 JSON Pointer 读取的业务 JSON，header/metadata 来源仍可使用。静态和动态检测结果不可变；不同绑定不会改写共享检测器。

**会话配置。** 需要跨请求复用时加入：

```yaml
session:
  identity: caller_scope
  on_missing: request
  max_id_bytes: 1024
  ttl: 30m
  max_sessions: 1024
  max_entries_per_session: 4096
  max_total_bytes: 67108864
  profiles:
    - namespace: conversations
      sources:
        - header: X-Session-ID
        - json: /metadata/session_id
        - metadata: /execution_session_id
```

上述数值也是声明 session 后的默认值。更完整的模型前缀与多来源示例见 [examples/session.yaml](../examples/session.yaml)。`profiles` 从前往后选第一个匹配项：`match.source_format` 精确匹配 CPA 输入格式，`match.requested_model_prefix` 匹配请求模型前缀，两者同时声明时须同时满足；省略 match 是兜底项。未命中任何 profile 的普通请求使用请求模式。

示例的前两项读取客户端提供的会话 ID，最后一项读取宿主的 `execution_session_id`。固定版本 CPA 会为每条 Responses WebSocket 连接生成一个 UUID，同连接多轮复用，重连后改变；普通 HTTP（包括 SSE）入口不会自动生成它。因此不能仅靠最后一项为普通 HTTP 开启跨请求复用。生成和传递过程见 [运维说明](OPERATIONS.md#execution-session-metadata)。

选定 profile 后依次取首个有效非空字符串，只有缺失、null、空字符串可以 fallback。非字符串、header 多值、控制字符、首尾空白、非法 UTF-8 或超过 max_id_bytes 都报错，不 trim、不自动解码。选中 ID 尚无映射也不会换来源；不同 ID 不建立别名。相同 namespace、可信调用方、精确 ID 可以跨 header/JSON/metadata 来源复用。

identity 只接受 CPA 已认证的 `metadata.caller_scope`，不接受客户端身份 header 替代。有候选 session ID 却无可信身份时拒绝。命中 profile 但所有来源缺失时，`on_missing: request` 用独立请求，`reject` 报错；有效的已登记历史引用可找回原会话。`previous_response_id` / `previous_interaction_id` 是续接引用，不能配置成 session source；未知、过期、冲突的引用是处理错误，不自动创建会话。完整生命周期与缓存说明见 [OPERATIONS.md](OPERATIONS.md)。

**跳过头。**

```yaml
bypass:
  headers:
    - name: X-Vibeguard-Skip
      value: "true"
    - name: X-Debug-Unmasked
      present: true
```

任一条件命中即跳过。name 大小写不敏感；value 大小写敏感，要求恰好一个值且完全相等，不自动 trim、拆分逗号或把其他真值识别为 true。`present: true` 仅检查存在，可接受空值或多值；同一条件不可同时写 value 和 present，也不接受 present: false。

跳过先于正文解析、协议选择、变量和 session 处理。它固定至 request.complete：请求、普通响应和所有流块均保持当前内容，包括看起来像标记的文本。CPA 认证、路由和之前的校验仍然执行。路径判断与可信代理注入见 [OPERATIONS.md](OPERATIONS.md) 和 [Caddyfile](../examples/Caddyfile)。

**资源限额。** 所有 `limits` 值为正整数，上界为 2^30；字节项以 bytes 计，max_inflight 以条目数计。不支持 0 表示无限制。默认值与范围来自 [internal/config/config.go](../internal/config/config.go)。

| 配置 | 默认值 | 约束对象 |
| --- | ---: | --- |
| `limits.request_bytes` | 8 MiB | 插件可见请求和脱敏后编码正文 |
| `limits.response_bytes` | 8 MiB | 普通响应正文、还原文本预算和最终编码结果 |
| `limits.event_bytes` | 1 MiB | 单个流事件及还原输出的事件预算 |
| `limits.tool_json_bytes` | 1 MiB | 单个工具 JSON 输入；普通响应/完整快照的编码后工具结果也检查 |
| `limits.request_state_bytes` | 16 MiB | 单请求缓存的脱敏正文加流状态计费 |
| `limits.max_inflight` | 256 | 活动请求索引，包含跳过/拒绝后等待完成回调的状态 |
| `session.ttl` | 30m | 无活动 lease 的会话空闲期限 |
| `session.max_sessions` | 1024 | 会话数量 |
| `session.max_entries_per_session` | 4096 | 单作用域的原文与标记映射数 |
| `session.max_total_bytes` | 64 MiB | 本实例映射总量计费，包含请求作用域 |

工具流保留的校验 buffer 容量计入 request_state_bytes，增量输出受 event_bytes 和本次输出状态预算约束；tool_json_bytes 不表示整条已交付流的累计输出上限。未声明 session 时，请求映射仍受固定的默认单作用域 4096 项、总量 64 MiB 限制，完成即释放。映射容量不足时可回收空闲会话，活动 lease 不被 TTL/LRU 淘汰；无法腾出容量时按请求错误处理。字节数是插件对自身数据的计费，不是 CPA 总 RSS 的硬上限。

还有以下固定边界，没有同名 YAML 配置键：

| 对象 | 上限 |
| --- | --- |
| 配置正文、每份规则来源 | 各 4 MiB |
| 外部/inline 规则来源 | 64 份、累计 16 MiB，合并后最多 4096 条规则 |
| 单变量、一次绑定值总量 | 4096 字节、64 KiB |
| 每代规则编译缓存 | 64 项、32 MiB 计费、空闲 TTL 10m |
| 历史引用索引 | 8192 项、8 MiB 计费 |
| RPC envelope（包含 base64 编码正文） | 64 MiB；请求钩子超限返回 HTTP 413 |
| 普通文本候选标记尾部 | 每字段 47 字节 |

RPC 限额在读取和解析 envelope 之前检查，超过时无法安全判断 RequestID 或跳过头，因此请求钩子始终返回 `rpc_size_limit` 业务拒绝，包括配置了 bypass 或 passthrough 的情况。响应及其他方法超限仍返回 RPC error。前置 Caddy 示例另将 HTTP 正文限制为 8 MiB，避免宿主先接收和复制超大正文；需要上传更大媒体时，应协调代理、插件正文限额和 RPC 编码开销。

配置更新为新请求创建新规则缓存；在途请求可继续持有旧代缓存。宿主在钩子前读取/归一化的正文、网络连接、HTTP 压缩和输出刷新由 CPA 管理，不在这些插件限额内。
