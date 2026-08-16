# Agent Loop

> 章节目标：从后端工程师视角理解 agent 的核心驱动循环——一次任务怎么被拆成 turn/step，模型和工具怎么来回，历史怎么从日志投影。配套可运行 Go 复现见 `main.go`。

## 一、核心概念：turn 和 step

agent loop 本质是一个**双层循环**：

| 概念 | 是什么 | 后端类比 |
|---|---|---|
| **turn（回合）** | 一次完整任务（可能问模型多次） | 一次"请求" |
| **step（步）** | 一次模型请求 + 它调用的工具 | 请求内部的一次迭代 |

**最容易搞错的点**：一个 step = **一次**模型请求 + 它调的工具。如果工具结果要再问一次模型，那是**下一个 step**（turn 外层循环），不是同一个 step 里循环。

## 二、完整事件序列（一次任务）

以"帮我处理任务"为例，模型先调 read、再调 bash、最后完成：

```
turn/start
  step/start (step 1)
    user/message           "帮我处理这个任务"
    assistant/chunk × N    "我先读文件"（流式分片）
    assistant/message      组装后的完整回复
    tool/call              read
    tool/result            read 的输出
  step/end (step 1)
  step/start (step 2)      ← 工具结果要再问模型，开新 step
    assistant/chunk × N    "再执行命令"
    assistant/message
    tool/call              bash
    tool/result            bash 的输出
  step/end (step 2)
  step/start (step 3)
    assistant/chunk × N    "完成了"
    assistant/message      （无 tool-call）
  step/end (step 3)
turn/end (completed)
```

**关键观察**：
1. `assistant/chunk` 是流式分片（模型一边生成一边落日志），`assistant/message` 是组装后的完整消息。
2. `tool/call` 和 `tool/result` 成对出现。
3. 模型返回**纯文本（无 tool-call）**的那一步，就是任务的终点。

## 三、三个关键函数

### 1. kick（外层驱动）

对应 `agent.ts:210-223`：

```go
func kick() {
    for {
        if !turn() { break }  // while (await this.turn()) {}
    }
}
```

就是"不停开 turn，直到 turn 说没活了"。

### 2. turn（一次任务）

对应 `agent.ts:246-330`。核心是外层循环 + preStep 门禁：

```
turn/start 落日志
第一个 step 前：preStep（claim 输入 + agent/pre-step waterfall，可 reject）
循环：
  step/start
  第一个 step 落 user/message（后续 step 靠 deriveMessages 投影工具结果）
  step() → 一次模型请求 + 执行工具
  step/end
  stepEnd==completed → turn/end 结束
  stepEnd==nil → 工具结果要再问模型 → 下一个 step
```

### 3. step（一次模型请求 + 工具）

对应 `agent.ts:332-399`：

```go
func step() *string {
    history := deriveMessages()        // 从日志投影历史
    msg := llm.Chat(history)           // 一次模型请求
    // ... 流式 chunk 落日志 + assistant/message 落日志 ...
    if len(msg.ToolCalls) == 0 {
        return "completed"             // 不调工具 → 完成
    }
    for _, call := range msg.ToolCalls {
        executeTool(call)              // 执行工具 + tool/result 落日志
    }
    return nil                          // 工具结果要再问模型
}
```

## 四、核心机制：deriveMessages（model-visible ⟺ logged）

这是整个框架的灵魂，对应 `session/src/index.ts:726-747`：

> 模型看到什么，永远能从日志投影出来。

- 日志是 append-only 的真相源
- 每次问模型前，用 `deriveMessages` 从日志**重新投影**历史（user/assistant/tool-result 消息）
- 这样"模型看到的上下文"和"日志"严格一致，不会丢、不会错

## 五、复现时踩的两个 bug（教学点）

### Bug 1：死循环（inbox 语义）

把 claim 取出的消息又放回 inbox，导致 inbox 永远非空、循环不结束。

**教训**：claim 是"从队列取出"，取出后消息进日志（session），**不再放回队列**。

### Bug 2：step 语义错误（把多次模型调用塞进一个 step）

第一版把"工具结果再问模型"写成了 step() 内部的 while 循环，导致 3 次模型调用全算同一个 step。

**教训**：真实源码里 step() 的 while 是**重试循环**（request-error 时 retry），不是"再问模型"的循环。"工具结果要再问模型"是 **turn 外层循环的下一个 step**。

这两个 bug 恰好暴露了对 agent-loop 最常见的两个误解，值得记住。

## 六、后端视角速记

| 概念 | 后端等价物 |
|---|---|
| turn | 一次请求处理 |
| step | 请求内一次"LLM 调用 + 工具调用" |
| 事件日志 + deriveMessages | 事件溯源 + 读模型投影 |
| assistant/chunk 流式 | 流式响应分片 |
| tool/call + tool/result | RPC 请求-响应成对 |
| agent/pre-step waterfall | 中间件/拦截器 |

## 七、deepseek-harness 源码对应

| 概念 | 源码位置 |
|---|---|
| kick（while turn） | `packages/core/agent-loop/src/agent.ts:210-223` |
| turn 状态机 | `packages/core/agent-loop/src/agent.ts:246-330` |
| preStep（claim + assemble + waterfall） | `packages/core/agent-loop/src/agent.ts:225-243` |
| step（buildRequest → stream → tool calls） | `packages/core/agent-loop/src/agent.ts:332-399` |
| buildRequest（agent/request waterfall） | `packages/core/agent-loop/src/agent.ts:407-495` |
| deriveMessages（投影历史） | `packages/core/session/src/index.ts:726-747` |

## 八、复现覆盖 + 未复现

**已覆盖**：
- turn/step 双层循环 + 完整事件序列
- preStep（claim + agent/pre-step waterfall 的 reject/enter）
- step（deriveMessages 投影 → 模型请求 → 流式 chunk → 工具执行）
- deriveMessages（model-visible ⟺ logged）

**未复现（真实实现有）**：
- `agent/request-error` waterfall（模型报错时 retry 重试）——这是 step() 里 while 循环的真实用途
- `buildRequest` 的 `agent/request` waterfall（请求前拦截）
- `max-tokens` 粘性（一旦某步达到上限，后续步不降级 turn 结果）
- 失败结构化（aborted / error，LlmError 保留事实，其他拍平成 errorChain）
- 取消（AbortController + signal.throwIfAborted 处处检查）
- inbox 的 next-turn / next-step 双 target 语义（demo 简化为单输入队列）

## 九、面试要点速记

1. agent loop = 双层循环：turn（一次任务）包 step（一次模型请求 + 工具）。
2. step 结束条件：模型返回纯文本不再调工具。
3. 工具结果要再问模型 = **下一个 step**，不是同一个 step 循环。
4. deriveMessages 从日志投影历史，保证 model-visible ⟺ logged。
5. assistant/chunk 流式分片 + assistant/message 组装，tool/call + tool/result 成对。
