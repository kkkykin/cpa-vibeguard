# cpa-vibeguard

cpa-vibeguard 是 CLIProxyAPI（CPA）的原生动态库插件：在请求送往上游之前，将规则命中的文本替换为随机标记；在普通响应、流式文本和工具参数中，按当前请求或会话的映射精确还原。

默认启用 Gitleaks 官方规则，支持 TOML、YAML 和 YAML inline 自定义规则。主要对话格式含 Chat Completions、Responses、Claude、Gemini 和 Interactions；图片/视频只处理明确的文本字段。实际范围见[覆盖矩阵](docs/COVERAGE.md)。**固定版本 CPA 的两个 Alpha Search HTTP 路由未经过这些钩子，查询不会被本插件脱敏。**

文档入口：[配置与规则](docs/CONFIGURATION.md) · [会话、升级与故障行为](docs/OPERATIONS.md) · [API 覆盖](docs/COVERAGE.md) · [基准](docs/BENCHMARKS.md) · [验收证据](docs/IMPLEMENTATION.md)。

**构建与安装。** 当前验证平台为 Linux/amd64，需要 Go 1.26.0、C 编译器和 Make。首次下载依赖需要网络；模块版本已固定，无本地 `replace`。

```sh
make build
```

产物为 `dist/cpa-vibeguard.so` 和 C 头文件 `dist/cpa-vibeguard.h`，CPA 运行时只需动态库。将 `.so` 放入 CPA 的插件目录，在已有 CPA 配置中加入：

```yaml
plugins:
  enabled: true
  dir: /opt/cpa/plugins
  configs:
    cpa-vibeguard:
      enabled: true
      priority: 100
      on_unknown_api: reject
      on_request_error: reject
```

把 `dir` 改成实际的绝对路径，并沿用 CPA 的入站认证、模型和上游凭据配置。插件不提供模型，也不替代 CPA 认证或协议翻译。最小配置使用官方规则、请求隔离、不追加提示、不跳过请求；需要跨请求复用标记时再配置 `session`。

[examples/cpa.yaml](examples/cpa.yaml) 是可编辑的宿主配置起点，使用仓库根目录下的 `dist`，另演示显式跳过头。先替换客户端 key 并补充自己的 CPA 上游配置。需要构建固定版本宿主时，可从仓库根目录运行：

```sh
bash scripts/build-host.sh
bin/cpa-host --config examples/cpa.yaml
```

启动后检查 CPA 的插件注册日志；单凭 `/healthz` 成功不能证明插件已加载。开发构建目录应与运行中的插件目录分开。使用指向 `dist` 的示例时，先停止宿主再重新构建，避免覆盖正在加载的 `.so`；升级步骤见[运维文档](docs/OPERATIONS.md)。

**兼容性。**

| 项目 | 已验证版本或范围 |
| --- | --- |
| 插件元数据版本 | `0.1.0-dev` |
| CPA | `v7.2.157`，参考提交 `09a29bd345bc44c473abe7fd07859e32df2ea543` |
| 原生接口 | C ABI 1 / RPC schema 6；注册时拒绝低于 6 的宿主 schema |
| Go / 平台 | Go `1.26.0`，Linux/amd64，`CGO_ENABLED=1`、`-buildmode=c-shared` |
| Gitleaks | `v8.30.1`，该版本默认启用 222 条官方规则 |
| 可选前置代理 | 示例已用 Caddy `v2.10.2` 验证；插件本身不依赖 Caddy |
| 其他 CPA 版本、操作系统和架构 | 尚未验收；不能仅凭 ABI 版本相同推断覆盖范围相同 |

**标记与会话。** 完整格式为 `[[redacted:v1:<32位小写十六进制ID>]]`，共 48 个 ASCII 字节，ID 为加密安全随机的 128 位值。同一作用域内，相同原文字节复用标记；默认每次请求独立。配置 session 后，作用域由 namespace、CPA 认证得到的 `caller_scope` 和 session ID 共同决定。秘密映射只存于内存，重启、替换插件或会话回收后需要重建上下文。

只还原完整、精确且属于当前作用域的标记；未知标记、裸 ID 和被模型改写的标记不猜测。`preserve_marker_hint: true` 可追加固定提示，但标记仍可能被模型缩写或改写。流式收到完整标记立即还原；正常结束前输出残缺文本尾部，异常 EOF/取消则丢弃尚未输出的候选尾部和解析缓冲。

**运行边界。** 插件按协议选择文本字段，保留模型/工具名、JSON key、数值、签名、加密内容和媒体载荷；不做 OCR、转写或对任意 JSON 字符串全局替换。计数接口统计的是脱敏后的内容。业务处理错误默认拒绝上游调用，但 CPA 在插件缺失、RPC 错误或熔断时可能跳过插件；本项目不承诺宿主故障时强制拒绝所有请求。响应还原失败会停止该次还原，具体行为见[故障说明](docs/OPERATIONS.md)。

**验证与开发。**

```sh
make check        # 全部单元测试、race、go vet、动态库构建
make integration  # 固定版 CPA/Caddy 构建、真实 C ABI 和宿主 + mock upstream 测试
make bench        # 规则、变量缓存、流式延迟与内存计费基准
```

集成测试使用本地合成请求和 mock upstream，无需真实模型账号；构建宿主与 Caddy 时可能下载依赖。测试覆盖协议往返、全部 49 个标记拆分位置、多轮会话、工具参数、重试、并发、取消、配置更新、重启与动态库替换。探针记录的 66 组入口结果在 [testdata/entrypoints.json](testdata/entrypoints.json)。测试只证明本地输入前缀稳定，不证明真实上游 prompt/KV 缓存命中。

实施约定保留在 [PLAN.md](PLAN.md)，逐项证据及最终验收记录见 [docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md)。
