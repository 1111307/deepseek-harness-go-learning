# Context Compaction

> 章节目标：从后端工程师视角理解「上下文压缩」——agent 长对话怎么在**不丢历史、不让模型超窗口**的前提下，把模型看到的上下文控制在预算内。配套可运行 Go 复现见 `main.go`。

## 一、核心思想：日志全量保留 + 投影层压缩

这是整个机制的灵魂，一句话：

> **不是删历史，而是"日志全量保留 + 投影时用摘要把窗口压回预算内"。**

后端视角的等价物：**事件溯源（Event Sourcing）**。

| 概念 | 说明 |
|---|---|
| `log`（append-only 日志） | 完整只增记录，原始事件一条不删，可重放、可审计 |
| `surface`（投影层） | 模型实际看到的"读模型"，压缩只改这里 |
| 压缩 | 把 surface 前面一段折叠成一条摘要节点，原始事件仍留 log |

所以关键不变量是：**模型看到什么（surface），永远能从日志（log）重建出来**。压缩只影响"怎么投影"，不影响"真相源"。

## 二、为什么需要压缩

大模型有**固定的上下文窗口**（token 上限），而 agent 长任务会不断累积历史：

- 多轮 tool-call 的中间结果
- 越来越长的对话
- 工具返回的大段输出

不压缩只有两条死路：要么历史撑爆窗口、请求被模型拒绝；要么丢弃早期历史、agent 失忆。压缩就是在"记忆"和"预算"之间找平衡。

## 三、触发：两种方式

| 触发 | 时机 | 说明 |
|---|---|---|
| **pressure（预防）** | 每个 step 前 | 本地估算当前占用超过窗口 80%，提前压 |
| **overflow（补救）** | 模型报"context window exceeded" | 强制压一次，然后重试请求 |

- pressure 是"估算到快超了主动压"，不发请求、不报错。
- overflow 是"真超了、模型拒了，压完重发"。

## 三.5 完整时序（三个角色：人 → harness → AI）

压缩发生时，多了一次「摘要调用」（harness → AI），它和"正式请求"是两次不同的调用。

### pressure 压缩（预防，最常见）

```
人(客户端)          harness 服务端                 AI 供应商
   │                    │                          │
   │ ① 用户发消息        │                          │
   ├───────────────────►│                          │
   │                    │ ② tokenMeter.measure      │
   │                    │   (估算 52 > 阈值 40)      │
   │                    │ ③ 触发 pressure 压缩       │
   │                    │   加锁 compaction/start    │
   │                    ├─────────────────────────►│ ④ 摘要请求
   │                    │                          │   (llm.stream 复用前缀)
   │                    │◄─────────────────────────┤ ⑤ 摘要文本
   │                    │ ⑥ 替换 surface            │
   │                    │   解锁 compaction/end     │
   │                    │                          │
   │                    │ ⑦ 组装正式请求(压缩后历史) │
   │                    ├─────────────────────────►│
   │                    │◄─────────────────────────┤ ⑧ tool-call
   │                    │ ⑨ 执行工具(grep/write)    │
   │                    ├─────────────────────────►│ ⑩ 带工具结果再调
   │                    │◄─────────────────────────┤ ⑪ 最终答案
   │◄───────────────────┤ ⑫ 流式回复               │
```

④⑤ 是压缩专用的摘要调用，⑦ 才是带压缩后历史问模型。

### overflow 压缩（补救，模型报错后）

```
人(客户端)          harness 服务端                 AI 供应商
   │                    │                          │
   │                    │ ① 发正式请求              │
   │                    ├─────────────────────────►│
   │                    │◄─────────────────────────┤ ② 报错: context window exceeded
   │                    │ ③ 触发 overflow 压缩      │
   │                    ├─────────────────────────►│ ④ 摘要请求
   │                    │◄─────────────────────────┤ ⑤ 摘要文本
   │                    │ ⑥ 替换 surface            │
   │                    │ ⑦ 重发请求(压缩后)        │
   │                    ├─────────────────────────►│
   │                    │◄─────────────────────────┤ ⑧ 正常返回
   │◄───────────────────┤ ⑨ 回复给用户              │
```

区别：pressure 是"还没发就压"（②估算触发）；overflow 是"发了被拒再压"（②报错触发）。

## 四、tokenMeter：怎么知道"现在多大"

不是每次请求带一个 token 数，而是**本地估算**：

```
当前大小 ≈ 最近一次 provider usage 锚点 + 之后 surface 的启发式增量
```

启发式估算：**每 4 个字符 ≈ 1 token**（`main.go` 里 `estimate()` 就是简化版）。

真实实现更精细：provider 每次响应会回传真实 `usage`（input/cache/output token），用它做"锚点"；两次响应之间用启发式估增量。而且只有"provider 真值 ≥ 启发式估算"时才采信 provider（防止缓存命中导致低估）。

## 五、压缩的五个步骤（加锁 → 摘要 → 替换 → 解锁）

对应 `main.go` 的 `Compact()`：

```
① 加锁     append compaction/start（日志括号，不是内存 mutex）
② 摘要     把 [0, cut) 压成一段（独立 LLM 调用）
③ 记录     append compaction/summary（log-only，不进 surface）
④ 替换     surface = [摘要节点] + 保留的尾部（唯一 surface 变更）
⑤ 解锁     append compaction/end
```

### 关键设计点

**1. 锁是日志里的一对括号**，不是内存 mutex

好处：崩溃后日志里留下"孤儿 `compaction/start`（无匹配 end）"，可被检测为 `busy`，而不是"谎称压完了但 surface 根本没变"。这是用 WAL 思想做分布式锁。

**2. 唯一的 surface 变更 = 那条摘要节点**

`compaction/start`、`summary`、`end` 全是 log-only，不上 surface。所以模型看到的变化只有一处：前面的 N 条消息变成了一条摘要。

**3. 切割边界要安全**

真实实现里，切割点不能落在一个"未闭合的 tool-call ↔ tool/result"中间，否则语义就断了。`main.go` 里简化成了纯 token 累计，真实代码用 `toolPairingBalancedBefore/After` 校验配平。

**4. 摘要复用会话前缀，保留 KV cache**

摘要的 LLM 调用复用会话自己的 system prompt + tools + messages 前缀，这样 provider 的 KV cache 不失效，摘要请求更便宜。

## 六、预算：阈值 80%、保留 16%

```go
Threshold = 窗口 × 80%   // 超过就压
Retain    = 窗口 × 16%   // 压的时候保留尾部
```

- 80% 是"别等到 100% 才动手"，留安全余量。
- 16% 是"压完别全压掉"，保留最近上下文（最近的消息对下一步最有价值）。
- 硬校验：`Retain < Threshold`，否则加载就报错。

## 七、投影：deriveMessages

压缩后，模型看到的历史 = **摘要节点（渲染成一条消息）+ 保留的尾部**。被 shadow 的原始事件仍在 log 里，所以重放是确定的——同样的 log 重放，永远得到同样的投影。

## 八、后端视角速记

| 概念 | 后端等价物 |
|---|---|
| append-only log + surface | 事件溯源（Event Sourcing + 读模型） |
| 压缩 | 对读模型做"折叠"，不动事件流 |
| tokenMeter 锚点+增量 | 增量计算 + 缓存最近快照 |
| 日志括号锁 | WAL 式的持久锁 |
| KV cache 复用前缀 | 请求前缀稳定 = 缓存命中 |
| 切割配平校验 | 事务边界一致性 |

## 九、deepseek-harness 源码对应

| 概念 | 源码位置 |
|---|---|
| 压缩引擎（compactIfNeeded 决策循环） | `packages/compaction/compaction-basic/src/index.ts:258-332` |
| 预算换算（threshold/retain） | `packages/compaction/compaction-basic/src/config.ts:144-167` |
| token 估算（锚点+增量，4 字符/token） | `packages/llm/token-meter/src/index.ts:116-310` |
| 选范围（safe edge） | `packages/compaction/compaction-basic/src/region.ts:98` |
| 原子段（加锁→摘要→替换→解锁） | `packages/compaction/compaction-basic/src/region.ts:152` |
| 投影（deriveMessages） | `packages/core/session/src/index.ts:726-747` |

## 十、面试要点速记

1. 压缩不是删历史，是"日志全量保留 + 投影层折叠"（事件溯源）。
2. 两种触发：pressure（80% 提前压）+ overflow（模型报错后压了重试）。
3. tokenMeter 是"锚点 + 增量"估算，不是请求里带 token 数。
4. 锁是日志括号（WAL 式），崩溃留孤儿可检测。
5. 摘要复用会话前缀保留 KV cache；切割边界不切断 tool-call 对。
6. 预算：80% 阈值、16% 保留，Retain 必须 < Threshold。

## 十一、Go 复现覆盖的真实机制

`main.go` 是完整复现（非简化版），逐段还原了真实源码的机制：

| 真实源码机制 | Go 复现对应 |
|---|---|
| 锚点 + 增量估算（`token-meter/index.ts:116-310`） | `TokenMeter.measure` + `onAssistantUsage`（provider usage 采信 + 保守校验） |
| 启发式估算（`estimate.ts`） | `estimateContent`（text/tool-call/tool-result 三种 block 定价 + 4 字符/token + block/role 开销） |
| usage 四桶汇总（`index.ts:44-49`） | `usageTokens`（input + cacheRead + cacheWrite + output） |
| 预算 + 校验（`config.ts:144-167`） | `resolveSpec`（80% 阈值 / 16% 保留 / Retain < Threshold 校验） |
| 选范围 + 配平（`region.ts:98-134`） | `selectCompactableRange` + `balanceAt` + `toolPairingBalancedBefore` |
| 压缩事务（`region.ts:152-254`） | `compactSurfaceRegion`（加锁→摘要→替换→解锁） |
| 摘要指令 + 复用前缀（`summarizer.ts`） | `summarize` + `frameSummary`（checkpoint preamble + 标签） |
| 摘要必须更小校验（`region.ts:374-378`） | `compactSurfaceRegion` 里的 `framedTokens >= shadowedTokens` 报错 |
| pressure/overflow 触发 + 重试循环（`index.ts:258-332`） | `compactIfNeeded` |

**仍未复现（真实实现有、demo 里省略）**：

- `compactNow`（人工 `/compact` 命令）和 `compactRegion`（指定范围强压）两个额外入口。
- overflow 触发的 `agent/request-error` 事件桥接（demo 里 `compactIfNeeded` 支持 overflow 分支，但 main 没演示报错触发）。
- `prune`（无模型删工具结果，`toolResultPruner`）——demo 里是 no-op 空壳。
- surface 稳定性校验（摘要期间 surface 不能被并发改写）。
- 摘要走真实 `ctx.llm.stream` 流式调用（demo 用字符串模拟返回）。

这些是"工程完整性"层面的细节，不影响对核心机制（锚点、配平、摘要、事务）的理解。
