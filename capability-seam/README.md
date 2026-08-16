# Capability Seam（Service Definition / Service Provider / Consumer）

> 章节目标：理解 deepseek-harness「一切皆插件」的地基——一个可替换的能力怎么被拆成三个独立演化的角色。这是前面所有章节默认你懂的底层结构。配套可运行 Go 复现见 `main.go`，canonical 例子是 `packages/shell` 三件套。

## 一、什么是 seam（能力接缝）

一个**可替换的能力**（bash 执行、文件系统、沙箱、web、subagent……）有三角色：

| 角色 | 是什么 | shell 例子 |
|---|---|---|
| **Service Definition** | Cordis `Service`，拥有 `ctx.<key>` 和词汇类型 | `dsh-shell` 的 `ShellExecutor` 抽象类 + `ctx.shell` |
| **Service Provider** | 注册/提供实现 | `dsh-bash-local`、`dsh-bash-sandbox` |
| **Consumer** | 模型/插件编程面对的东西 | `dsh-tool-bash` 的 `bash` 工具 |

**seam 指的是三角色整体**，不是某一个角色。单独一个角色不是 seam——要叫就按角色名（"Service Definition"、"provider"、"Consumer"）来叫。

**为什么拆**：三个关注点以**不同的速率、不同的原因**变化——契约（能力是什么）、实现（怎么跑）、消费者 API（模型面对什么）。捆在一个包里就耦合了这些变化速率：把本地 executor 换成沙箱 executor 会连带搅动模型看到的工具 schema，即使模型面对的契约根本没变。

## 二、最关键的一条：Service Definition 是 Service 抽象类，不是 interface

这是理解 Cordis 能力 seam 的第一道坎：

> Service Definition 是 Cordis `Service`（抽象类，如 `ShellExecutor`），**绝不是 TypeScript `interface`**。

为什么必须是类：Cordis 的服务要 `super(ctx, 'shell')` 把自己注册进 context，interface 做不到"注册进 context"这一步。它拥有 `ctx.<key>`（通过 declaration merging 把 `shell: ShellExecutor` 加进 `Context` 接口），并携带词汇类型（`ShellExecRequest`/`ShellExecSpec`/`ShellRunResult`）。

**后端类比**：Service Definition ≈ Java/Go 的接口 + 服务注册键。抽象类 `ShellExecutor` 定义契约（`resolve`/`run`/`start` 三个抽象方法），`ctx.shell` 是这个接口在注册表里的键。

## 三、注册即 effect + 注入

- **Provider 注册**：子类 `extends ShellExecutor`，构造函数 `super(ctx, 'shell')` 把自己挂到 `ctx.shell`。**一个 context 只能有一个实现**，挂第二个抛 duplicate-service（Cordis 标准行为）。
- **Consumer 注入**：`inject: ['shell']`（`tool-bash/src/index.ts:31`），Cordis 的 fiber 会 **pend 直到服务存在**——声明式依赖等待，不是轮询。

**后端类比**：Provider 注册 ≈ 服务发现/依赖注入容器的 `bind(key, impl)`；Consumer 注入 ≈ 构造器注入 `@Inject`。Cordis 的 `inject` 数组就是声明式的依赖声明，运行时由容器解析依赖顺序。

## 四、resolve(request): Spec——显式默认值

这是 AGENTS.md 反复强调的模式（`dsh-shell` 是模板）：

> 默认值是 owning 实现里的一个**显式 `resolve(request): Spec` 步骤**，绝不是 `run()` 里隐藏的 `?? default`。

```
Consumer 构造 request（可选字段 workdir/timeoutMs 缺省）
        ↓
ctx.shell.resolve(request)  →  填充默认值 + cap 上限 → Spec（全显式）
        ↓
ctx.shell.run(spec)         →  收到显式值，绝不重新默认
```

`LocalBashExecutor.resolve`（`bash-local/src/index.ts:146-171`）：workdir 填 `config.cwd`，timeoutMs 填 `config.timeoutMs` 并 cap 在 `config.maxTimeoutMs`。`run()` 只收 Spec，不碰 request。

**后端类比**：request → spec 就是"入参 DTO → 经过校验/默认值/上限 clamp → 领域对象"的边界转换。默认值收敛在 resolve 一个地方，run 拿到的永远是合法完整值。

## 五、换 Provider 不动 Consumer（核心价值）

`sandbox` 章节的 `SandboxBashExecutor` 就是活例子：

- `SandboxBashExecutor extends LocalBashExecutor`（`bash-sandbox/src/index.ts:44`），复用本地进程生命周期，只做三件事：
  1. **覆盖 `sandboxMode`**（`bash-sandbox/src/index.ts:75-77`）——声明默认 mode，tool 层读它决定是否广告升级字段。
  2. **覆盖 `resolve`**（`bash-sandbox/src/index.ts:84-86`）——stamp 默认 sandbox policy。
  3. **覆盖 `run`**（`bash-sandbox/src/index.ts:88-114`）——`ctx.sandbox.confine()` 包装 argv，再复用 `runArgv`，附加 sandbox facts。

- `tool-bash`（Consumer）**零改动**：它的 execute 仍然 `ctx.shell.resolve(request)` + `ctx.shell.run(spec)`，schema 完全不变。

**后端类比**：这就是策略模式 / 依赖倒置的完整形态——Consumer 依赖抽象（`ShellExecutor`），Provider 是可插拔实现，换实现不碰调用方。

## 六、后端视角速记

| 概念 | 后端等价物 |
|---|---|
| Service Definition（抽象类 + ctx.key） | 接口 + 服务注册键 |
| Provider extends Service + super(ctx,key) | 依赖注入 bind(key, impl) |
| Consumer inject: ['shell'] | 构造器注入 @Inject |
| 重复注册抛错 | 重复 bean 定义报错 |
| resolve(request): Spec | DTO → 领域对象（默认值 + clamp 收敛一处） |
| 换 Provider 不动 Consumer | 策略模式 / 依赖倒置 |

## 七、deepseek-harness 源码对应

| 概念 | 源码位置 |
|---|---|
| Service Definition 抽象类 `ShellExecutor` | `packages/shell/shell/src/index.ts:65-101` |
| `ctx.shell` declaration merging | `packages/shell/shell/src/index.ts:40-44` |
| `ShellExecRequest` / `ShellExecSpec` 类型 | `packages/shell/shell/src/types.ts:38-79, 86-110` |
| `LocalBashExecutor`（Provider） | `packages/shell/bash-local/src/index.ts:102` |
| `LocalBashExecutor.resolve` | `packages/shell/bash-local/src/index.ts:146-171` |
| `SandboxBashExecutor extends LocalBashExecutor` | `packages/shell/bash-sandbox/src/index.ts:44` |
| `SandboxBashExecutor.resolve` 覆盖 | `packages/shell/bash-sandbox/src/index.ts:84-86` |
| `SandboxBashExecutor.run`（confine 包装） | `packages/shell/bash-sandbox/src/index.ts:88-114` |
| Consumer `tool-bash` inject | `packages/shell/tool-bash/src/index.ts:30-31` |
| Consumer `execute`（resolve → run） | `packages/shell/tool-bash/src/index.ts:330-390` |
| 三角色定义（权威说明） | `.agents/notes/implemented/architecture/2026-06-13-capability-seams.md` |
| glossary 里的 seam 定义 | `docs/glossary.md:9` |

## 八、复现覆盖 + 未复现

**已覆盖**：
- 三角色结构（Service Definition 接口 / Provider 注册 / Consumer 注入）
- 注册即 effect（一个 key 一个实现，重复 fail-loud）
- Consumer 只依赖接口，不 import provider 类型
- resolve(request): Spec 显式默认值 + cap
- 换 Provider 不动 Consumer（local → sandbox）
- sandbox provider 覆盖 resolve（stamp 默认 policy）+ run（confine 包装 argv）

**未复现（真实实现有、demo 里省略）**：
- Cordis 的 `inject` fiber pend 机制（demo 用 map.get，非声明式依赖等待）
- `ctx.shell` 的 declaration merging（TS 类型合并，Go 无对应）
- `static Config` + schemastery schema 默认值填充（demo 用构造函数参数）
- 真实的 `ctx.subprocess` 依赖链（bash-local inject subprocess，demo 里模拟执行）
- 设置命名空间 `SHELL_SETTINGS_NAMESPACE` + settings section 热更新
- `run`/`start` 的完整生命周期（timeout/deadline/abort 分类、进程组 SIGTERM→SIGKILL）

## 九、面试要点速记

1. 能力 seam 是三角色：Service Definition（契约+ctx.key）、Service Provider（实现）、Consumer（消费），seam 指整体。
2. Service Definition 是 Cordis `Service` 抽象类，**不是 interface**——因为要注册进 context。
3. 拆三角色的原因：契约/实现/消费者 API 三者变化速率不同，捆一起会耦合变化。
4. Provider 注册 `super(ctx,key)`，一个 key 一个实现；Consumer `inject` 声明依赖，容器 pend 等待。
5. resolve(request): Spec 是显式默认值步骤，run 收显式值绝不重新默认。
6. 换 Provider 不动 Consumer：sandbox 替换 local，tool schema 零改动（策略模式/依赖倒置）。
