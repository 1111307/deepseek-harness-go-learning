# Human-in-the-loop

> 章节目标：从后端工程师视角理解「人在环上」（HITL）——一次 agent 任务里，服务端如何在不中断循环、不放任风险的前提下，停下来等人工决策。配套可运行 Go 复现见 `main.go`。

## 一、一句话定义

Human-in-the-loop = 自动化流程跑到某个节点，**停下来等人做决定，拿到答案再继续**。

后端视角的等价物：工作流引擎里的「用户任务节点」（BPMN user task）。它解决的不是"怎么让人参与"，而是"怎么在**不中断循环、不放任风险**的前提下让人参与"。

## 二、为什么 agent 必须有它

纯自动化遇到这些情况就不该自己走：

| 场景 | 例子 |
|---|---|
| 不可逆操作 | 删库、rm -rf、发邮件、扣钱、提交代码 |
| 权限升级 | 模型想在沙箱外执行命令 |
| 信息不足 | 需求有歧义，模型猜不如问 |
| 置信度不足 | 模型没把握，让人兜底 |

没有 HITL，agent 只有两个结局：权限阉割到啥都干不了，或者放任它乱来。HITL 是「全自动」和「全手动」之间的闸门。

## 三、完整链路（一次任务，3 次循环）

以「帮我看看这个 Go 服务有没有性能隐患」为例：

```
人(客户端)          harness服务端                AI(供应商)
─────────         ──────────────              ──────────────
  │ 发任务            │
  ├─────────────────►│  ① 调 SDK
  │                  ├──────────────────────────►│  "我来处理"
  │                  │◄──────────────────────────┤  bash: ls -la
  │                  │  ③ 本地执行 ls → 喂回       │
═══════════════ 第 1 次循环（终端命令）═══════════════
  │                  │  ④ 再调 SDK（带 ls 结果）   │
  │                  ├──────────────────────────►│  "想跑 pprof 分析"
  │                  │◄──────────────────────────┤  ask_user_question
  │                  │  ⑥ 挂起，SSE 推问题帧       │
  │◄─────────────────┤  (带 rpcId)               │
  │  点"静态看"       │                           │
  ├─────────────────►│  POST /api/respond 解挂   │
═══════════════ 第 2 次循环（HITL 询问人类）═══════════════
  │                  │  ⑦ 再调 SDK（带答案）       │
  │                  ├──────────────────────────►│  "静态扫描泄漏"
  │                  │◄──────────────────────────┤  bash: grep + write
  │                  │  ⑨ 执行 grep、写文件 → 喂回 │
═══════════════ 第 3 次循环（命令 + 写脚本）═══════════════
  │                  │  ⑬ 再调 SDK               │
  │                  ├──────────────────────────►│  "完成了"
  │                  │◄──────────────────────────┤  纯文本，无 tool-call
  │                  │  ⑮ turn/end，任务完成      │
```

## 四、核心认知：大模型不会"执行任务"

这是最容易误解的一点：

- **AI（大模型）是无状态的文本生成器**，每次调用只返回一段文本。
- 它不能自己读文件、执行 bash、上网——**这些是 harness 本地进程干的**。
- 所谓"我做完了"，不是 AI 干完全部活后的汇报，而是**某一轮**它判断"不用再调工具了"，返回纯文本。

真正的循环是：`AI 出指令 → harness 干活 → 结果喂回 → AI 再出指令`，直到 AI 不再要工具。

## 五、HITL 的工程本质：挂起 + 关联 ID

### 5.1 挂起 = 阻塞，不是空转

```go
// ❌ 空转（busy wait）：占 CPU 烧核
for { if answerArrived(id) { break } }

// ✅ 阻塞：goroutine 睡着，CPU 让给别的活
outcome := <-ch
```

挂起的是「这一个 step 的 goroutine」，只占几 KB 栈内存，不占 CPU，不阻塞其他会话。

### 5.2 双通道 + rpcId 关联

```
下行（服务端→前端）：SSE/流，单向，推问题帧（带 rpcId）
上行（前端→服务端）：独立 POST /api/respond，echo 同一个 rpcId
```

SSE 本身单向，所以"等"不是"SSE 等回复"，而是"服务端挂一个 pending，等另一条独立 HTTP POST 来 resolve"。

关键：**一个 rpcId 对应一个 chan**（`map[rpcId]chan`），不是全局 channel——否则多会话并发时答案会串扰。这跟后端异步应答 / MQ 请求-应答的 correlation id 一个道理。

### 5.3 fail-closed 默认值

没有 answerer 能回答（如 headless 环境没接 UI）时，默认 `unavailable`（拒绝），不是放行。安全系统宁可误拒，不可误放。

### 5.4 超时与崩溃恢复

- 挂起期间人永远不答 → 超时兜底 fail-closed 拒绝
- 进程崩溃重启 → 日志里 `asked` 无 `decided` = 孤儿，按拒绝处理

## 六、两种 HITL 形态

| 形态 | 触发方 | 例子 |
|---|---|---|
| 审批型（系统问人） | 系统判断"这步要确认" | 危险工具执行前 accept/reject |
| 询问型（模型问人） | 模型主动调 `ask_user_question` 工具 | 需求歧义时让用户选方案 |

实现共用一个机制（挂起 + 双通道 + rpcId 关联），区别是**谁发起**：审批是门禁代码发起，询问是模型把"问人"当成一个普通工具调用，答案作为普通 tool result 回到循环。

## 七、deepseek-harness 源码对应

| 概念 | 源码位置 |
|---|---|
| 审批服务（挂起 + 策略 + 审计） | `packages/interaction/user-approval/src/index.ts:257-345` |
| ask_user_question 工具（模型问人） | `packages/interaction/tool-ask-user/src/index.ts` |
| 审批问题帧 + 应答契约（POST /api/respond） | `packages/host/apiproxy/src/api/approvals.ts:1-22` |
| 挂起表（PendingQuestion：rpcId + resolve/reject） | `packages/host/apiproxy/src/api-proxy.ts:704-733` |

## 八、Go 复现覆盖的真实机制

`main.go` 是完整复现，四个演示场景覆盖了 HITL 的全部机制：

| 演示 | 验证的机制 |
|---|---|
| 演示 1：waterfall 链 | read 工具被白名单 answerer **短路放行**，bash 工具**委托**给 UI answerer 挂起问人 |
| 演示 2：policy 持久化 | `approval/policy` 走 durable 日志，`effectivePolicy` 从日志**回放**（非内存字段） |
| 演示 3：询问型 | `ask_user_question` 工具，模型主动问人，provider 挂起等答案 |
| 演示 4：多会话 scope | 两个 agent 并发审批，`pending` 按 agent.id 隔离，不串扰 |

逐段对应的真实机制：

| 真实源码机制 | Go 复现对应 |
|---|---|
| `approval/request` waterfall 链（多个 answerer，短路/委托，`index.ts:317-328`） | `waterfall` + `Answerer`（返回 handled 短路 / 委托） |
| `approval/policy` durable 持久化 + 回放（`index.ts:112-118, 142-147`） | `setPolicy` 写日志 + `effectivePolicy` 回放 |
| 审计双事件 asked/decided（`index.ts:267-274`） | `appendEvent("approval/asked"/"approval/decided")` |
| 策略 `ask`/`never`（`index.ts:312`） | `decide` 里 policy 检查，never 先于 answerer |
| abort → cancelled（`index.ts:306, 330-343`） | `select` 的 `<-req.signal` 分支 |
| 归一化（非白名单 → unavailable）（`index.ts:325`） | `outcomeWhitelist` 校验 |
| 询问型 `ask()`（`user-questions/src/index.ts:92-139`） | `askUserQuestion`（空问题/无 provider 报错，provider 挂起） |
| 作用域路由 `scopeTarget`（`core/scope/src/index.ts`） | `pending` 按 agent.id 隔离 |
| 挂起表 map[rpcId]（`api-proxy.ts:704-733`） | `pending` map + chan |
| 双通道 + rpcId 关联（`approvals.ts:1-22`） | 下行推 + `respond` echo rpcId |

**已全部复现**（无省略）。仅剩"工程完整性"层面的差异：真实 `approval/request` 是 Cordis 的异步 waterfall（含 `next()` 语义和异常容器），demo 用同步 `Answerer` 链表达同样的短路/委托语义；真实 `scopeTarget` 是 scope 树的路由 filter，demo 用 agent.id 平铺表达多会话隔离。

## 九、面试要点速记

1. HITL = 中间件里 `<-ch` 阻塞等带关联 ID 的异步应答，不新起请求、不放任风险。
2. 大模型不执行任务，只出指令；执行是 harness 本地干的。
3. 任务完成 = 某轮模型返回纯文本、不再调工具。
4. 双通道（下行 SSE 推 + 上行 POST 答）+ rpcId 关联 + fail-closed + 审计双事件。
5. 策略分级：ask（走 answerer 链）/ never（确定性拒绝），无 answerer 时 fail-closed。
