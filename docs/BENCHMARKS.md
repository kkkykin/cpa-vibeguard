# 基准记录

2026-09-12 在 Linux/amd64、Intel Core i7-8550U @ 1.80 GHz、Go 1.26.0 上运行。业务实现基线为 `ad57993`，使用本次提交中的基准。依赖为 Gitleaks v8.30.1 / CPA v7.2.157；这些微基准直接调用 Go 核心，不启动 CPA。

复现命令：

```sh
make bench
# 等价于：
go test ./internal/rules ./internal/restore -run '^$' -bench . -benchmem -benchtime=200ms -cpu=1
```

`-cpu=1` 固定 GOMAXPROCS 为 1，不固定 CPU 频率或排除同机负载。下面是一次运行的观测值，用于识别成本与回归，不是吞吐承诺。计时不含基准外的规则加载；扫描过程中仍包含实际检测和 span 合并。

| 基准 | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| RuleScan/custom/clean | 68,236 | 153.82 | 15,424 | 8 |
| RuleScan/custom/match | 154,634 | 68.04 | 65,458 | 300 |
| RuleScan/builtin/clean | 861,450 | 12.18 | 15,936 | 9 |
| RuleScan/builtin/match | 983,666 | 10.72 | 67,472 | 306 |
| VariableBinding/hit | 1,579 | — | 552 | 9 |
| VariableBinding/miss | 29,520 | — | 61,654 | 134 |
| FramerRecordByteFragments/4096 | 165,018 | 24.92 | 32,617 | 18 |
| FramerRecordByteFragments/65536 | 2,586,700 | 25.34 | 506,752 | 27 |
| TextMarkerLatency | 222 | 216.19 | 112 | 3 |
| LongStream/256-chunks | 5,703,866 | 52.21 | 3,608,871 | 35,569 |
| LongStream/4096-chunks | 90,217,873 | 52.80 | 57,663,426 | 567,447 |
| ToolJSONFragments | 2,456,274 | 5.04 | 547,083 | 38,647 |

**输入与计时范围。** 规则扫描使用约 10 KiB 的普通代码文本；命中场景额外追加合成样本。custom 只启用一条规则，builtin 启用固定版本的 222 条官方规则。每次扫描检查是否存在预期命中，不使用真实凭据。变量绑定 hit 重用同一 header 值；miss 每次生成新值并编译，计时包括样本字符串格式化。运行结束时缓存分别为 1 和 64 项，基准检查条目数和计费字节未越界。

Framer 将一条 SSE data record 按每字节一次 `Push` 输入，JSON 字符串中含 4 KiB 或 64 KiB 的 `}`。输入增大 16 倍，耗时约增大 15.7 倍；增量完成判断避免对每个字符串内的右括号反复解析整个增长中的记录。每次运行核对恰好产出一条记录。

TextMarkerLatency 把一个 48 字节标记拆成 47+1 两段，测量两次 `Text.Push` 和一次查表还原。完整标记在第二次调用内输出，尾部随即清空。222 ns 是这段同步代码的平均成本，不包含等待最后一个字节、RPC/base64、外层 JSON/SSE 编解码、宿主翻译或网络延迟。

LongStream 每块包含约 1 KiB 文本及 Chat JSON 包装，每两块跨一个标记；一次 op 从新建流开始，处理 256 或 4096 块，再处理正常结束并清理。ToolJSONFragments 把约 12 KiB 的工具参数按 23 字节分片，含中文、完整标记和超过 2^53 的整数；一次 op 处理整个参数并核对完成后释放校验缓冲。

**保留状态与内存。** 以下是每次 `Push` 返回后从 `Buffered()` 采样的最大值：

| 基准 | 指标 | 字节 |
| --- | --- | ---: |
| TextMarkerLatency | peak-tail-B | 47 |
| LongStream/256-chunks | peak-state-B | 546 |
| LongStream/4096-chunks | peak-state-B | 546 |
| ToolJSONFragments | peak-tool-B | 16,422 |

`peak-state-B` 是流状态的计费值，包括尾部、字段定位信息、已关闭字段记录和解析缓冲；不含宿主缓存的请求、session 映射和调用中临时生成的输出。`peak-tool-B` 包括参数校验 buffer 的容量、JSON 转义残片及解析栈。普通文本的 47 字节上限只约束候选标记尾部，工具和事件缓冲各有独立上限。

`B/op` 是一次操作的累计堆分配量。长流的累计分配会随块数增加，即使调用间保留的状态大小不变。以上值均不是 Go 堆峰值或整个 CPA 进程的 RSS；本记录未测量进程 RSS，也不据此推算最大部署并发。容量应结合实际规则、并行输出/工具数、映射内容、宿主开销和配置限额验证。

基准源文件：[规则与变量](../internal/rules/benchmark_test.go)、[还原与流式](../internal/restore/benchmark_test.go)。边界断言及正确性回归仍由对应包的单元测试和真实宿主集成测试负责。
