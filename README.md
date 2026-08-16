# DeepSeek Harness Go 源码学习笔记

> 用 Go 语言完整复现 deepseek-harness（一个插件化 agent harness）的核心机制，每章对照真实源码逐段还原，边读源码边用 Go 写出来跑通。目标：从后端工程师视角吃透 agent 系统的关键设计。

## 目录结构

```
deepseek-harness-go-learning/
├── README.md                 # 本文件（总目录）
├── human-in-the-loop/        # 第 1 章：人在环上（HITL）
│   ├── main.go               # 可运行复现（go run main.go）
│   └── README.md             # 解析笔记 + 机制对照表
├── context-compaction/       # 第 2 章：上下文压缩
│   ├── main.go
│   └── README.md
├── agent-loop/               # 第 3 章：agent 核心驱动循环
│   ├── main.go
│   └── README.md
├── sandbox/                  # 第 4 章：进程隔离 + 文件策略 + 升级
│   ├── main.go
│   └── README.md
├── capability-seam/          # 第 5 章：Service Definition / Provider / Consumer
│   ├── main.go
│   └── README.md
└── plugin-runtime/           # 第 6 章：一切皆插件 + 注册即 effect + 作用域
    ├── main.go
    └── README.md
```

每章都是 **main.go（可运行复现）+ README.md（解析笔记）** 配套，README 里的每个机制都标注了真实源码的 `file:line`。

## 章节列表

### 第 1 章：Human-in-the-loop（人在环上）

**讲什么**：agent 任务里，服务端如何停下来等人工决策（accept/reject / 回答问题），既不中断循环、也不放任风险。

**复现的机制**（对照 `packages/interaction/*`）：

- `approval/request` 的 waterfall 链（多个 answerer，短路/委托 + fail-closed）
- `approval/policy` 的 durable 持久化（写日志 + 回放，非内存字段）
- 审计双事件 `approval/asked` + `approval/decided`
- 策略分级 `ask` / `never`
- 询问型 HITL（`ask_user_question` 工具，模型主动问人）
- 多会话 scope 路由（`scopeTarget`，审批问题路由到正确 agent）
- 双通道通信（下行 SSE 推 + 上行 POST 答）+ rpcId 关联

**运行**：`cd human-in-the-loop && go run main.go`

### 第 2 章：Context Compaction（上下文压缩）

**讲什么**：长对话如何在不丢历史、不让模型超窗口的前提下，把模型看到的上下文控制在预算内。

**复现的机制**（对照 `packages/compaction/*` + `packages/llm/token-meter/*`）：

- 事件溯源（append-only 日志 + 投影层压缩）
- tokenMeter 锚点机制（provider usage 采信 + 保守校验 + 增量）
- 启发式估算（4 字符/token + block/role 开销）
- 预算（80% 阈值 / 16% 保留 + 校验）
- 选范围 + tool-call 配平切割
- 压缩事务（加锁→摘要→替换→解锁，日志括号锁）
- 结构化摘要（复用前缀保留 KV cache + 摘要必须更小校验）
- pressure/overflow 两种触发 + 重试循环

**运行**：`cd context-compaction && go run main.go`

### 第 3 章：Agent Loop（核心驱动循环）

**讲什么**：一次任务怎么被拆成 turn/step 双层循环，模型和工具怎么来回，历史怎么从日志投影。

**复现的机制**（对照 `packages/core/agent-loop/*` + `packages/core/session/*`）：

- turn/step 双层状态机（turn = 一次任务，step = 一次模型请求 + 工具）
- preStep（claim 输入 + agent/pre-step waterfall，reject/enter）
- step（buildRequest → 流式 chunk → 工具执行 → 返回 completed/null）
- deriveMessages（append-only 日志投影历史，model-visible ⟺ logged）
- 结束条件（模型返回纯文本不再调工具 → step completed → turn 结束）

**运行**：`cd agent-loop && go run main.go`

### 第 4 章：Sandbox（进程隔离 + 文件策略 + 升级）

**讲什么**：如何隔离 agent 生成的不可信命令、如何约束文件写入、如何安全升级权限。

**复现的机制**（对照 `packages/sandbox/*` + `packages/fs/fs-sandbox/*`）：

- 两层沙箱：内核级进程约束（confine 包装 argv）vs 进程内 fs 栅栏（canonicalize-then-contain）
- 平台 runner 链选择 + 功能探针仲裁 + fail-closed（`SANDBOX_UNAVAILABLE`）
- 三种 profile 包装（bwrap / Landlock / Seatbelt）
- writableRoots（workspace-write 可写根集合 + canonical 去重）
- 进程内栅栏 checkedTarget 四分支 + isPathUnder（词法 + 身份回退）
- 升级阶梯（WIDER_MODES 严格更宽 + 审批 fail-closed）
- 策略解析三层优先级 + sandbox/mode 日志 fold

**运行**：`cd sandbox && go run main.go`

### 第 5 章：Capability Seam（Service Definition / Provider / Consumer）

**讲什么**：一个可替换的能力怎么被拆成三个独立演化的角色——「一切皆插件」的地基。

**复现的机制**（对照 `packages/shell/*` 三件套）：

- 三角色结构（Service Definition 接口 / Provider 注册 / Consumer 注入）
- Service Definition 是 Cordis Service 抽象类，不是 interface
- 注册即 effect（一个 key 一个实现，重复 fail-loud）+ Consumer 只依赖接口
- resolve(request): Spec 显式默认值 + cap
- 换 Provider 不动 Consumer（local → sandbox）

**运行**：`cd capability-seam && go run main.go`

### 第 6 章：Plugin Runtime（一切皆插件 + 注册即 effect + 作用域）

**讲什么**：vendored Cordis 插件框架——前面所有章节赖以生长的运行时骨架。

**复现的机制**（对照 `vendor/cordis/src/*` + `packages/core/scope/*`）：

- Context 服务仓库（ctx.<key>）+ inject 依赖声明 + 重复注册 fail-loud
- 注册即 effect（ctx.effect 返回 disposer，卸载反转顺序）
- 五种 dispatch（emit / waterfall / serial / bail，waterfall 重点）
- waterfall 的 next() 委托 + 短路 + 值传播
- 作用域链（scopeParents）+ 事件向上流动、绝不向下

**运行**：`cd plugin-runtime && go run main.go`

## 全章回顾

六章从外到内覆盖了 deepseek-harness 的核心设计：

1. **human-in-the-loop** — 服务端如何停下来等人工决策
2. **context-compaction** — 长对话如何在预算内压缩
3. **agent-loop** — turn/step 双层循环驱动模型-工具来回
4. **sandbox** — 两层隔离（内核进程约束 + 进程内 fs 栅栏）+ 升级
5. **capability-seam** — 能力拆成 Service Definition / Provider / Consumer
6. **plugin-runtime** — 一切皆插件 + 注册即 effect + 作用域

建议阅读顺序：6（骨架）→ 5（能力接缝）→ 3（循环）→ 1（人环）→ 2（压缩）→ 4（沙箱），由底层结构到具体机制层层递进。

## 约定

- 每章 main.go 是**完整复现**（非简化），能对着真实源码逐段核对
- README 里每个机制标注源码 `file:line`
- 复现过程中发现的 Go 层面 bug（值传递、状态脱节等）会写进 README 作为教学点
- 目标是"看懂设计"，不是"翻译 TS 代码"——Go 复现保留真实语义，但不逐行搬
