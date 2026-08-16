# Plugin Runtime（一切皆插件 + 注册即 effect + 作用域）

> 章节目标：理解 deepseek-harness 的运行时骨架——vendored Cordis 插件框架。前面所有章节（agent-loop、compaction、sandbox、capability-seam）都是在这个骨架上长出来的。配套可运行 Go 复现见 `main.go`。

## 一、Cordis 在五个概念里

`docs/cordis-primer.md` 的开篇就是这五个概念，是理解一切的入口：

1. **插件是实现了 Service 的对象**——可以是一个带 `inject`/`apply(ctx)` 字段的函数，或一个 `Service` 子类。
2. **Context 是服务仓库**——服务从 context 认领一个稳定的 `ctx.<key>`（如 `ctx.tools`、`ctx.llm`、`ctx.sessions`）；其他插件按 key 找服务，不 import 具体实现。
3. **用 `inject` 声明依赖**——声明了所需服务的插件会**等那些服务存在才启动**，所以加载顺序通过服务需求表达，而非手动 boot 顺序。
4. **Typed Events 通信**——服务用 declaration merging 声明事件名，然后按 `emit`/`waterfall`/`parallel`/`serial` 分派。
5. **注册是可逆的 effect**——prompt 段、工具 schema、adapter、provider、listener 都通过 `ctx.effect()`/`ctx.on()` 安装，所以 reload/teardown 能按序撤销。

## 二、Context 是服务仓库 + Proxy

`Context`（`vendor/cordis/src/context.ts:42`）在运行时是一个 **Proxy**：正常的属性读走服务解析器；`extend()`/`isolate()`/`intercept()` 创建**作用域子 context**，不改父。

- **`extend(meta)`**（`context.ts:99`）：原型继承父的所有属性，meta 的 own 属性遮蔽继承的。
- **`isolate(name, label)`**（`context.ts:121`）：为某个服务名创建**独立服务作用域**——在返回的 context 下，读/写 `name` 服务解析到新 label，所以能给一个 key 提供不同实现而不影响父作用域。
- **`intercept(name, config)`**（`context.ts:139`）：给某服务加 per-service intercept config（祖先条目先合并）。

**后端类比**：Context ≈ Spring IoC 容器 + Bean 作用域。`isolate` ≈ 子容器 / 作用域隔离（类似 web 请求作用域），`extend` ≈ 子容器继承父 Bean。

## 三、Service：super(ctx, name) 注册

`Service`（`vendor/cordis/src/service.ts:11`）是暴露命名 API 到 `ctx` 的基类。子类构造函数调 `super(ctx, name)`（`service.ts:42`）：

- 立即注册：`ctx.reflect.provide(name, self)`。
- **随 owning fiber 自动移除**——这是"注册是可逆 effect"在服务层面的体现。

这解释了 capability-seam 章节里的核心约束：Service Definition 是 `Service` 抽象类（`ShellExecutor extends Service`），**不是 interface**，因为要 `super(ctx, 'shell')` 注册进 context。

## 四、注册即 effect：disposer 反转顺序

`ctx.effect()`（`vendor/cordis/src/fiber.ts:418`）是整个框架的可逆性根源：

```
body 立即执行，body 里收集的 disposer 被记录
返回的 disposer 调用时按【反转顺序】跑（fiber.ts:431 的 .reverse()）
调用两次 no-op
```

`ctx.on()` 本质就是 `ctx.fiber.effect(() => { hooks.push(listener); return () => unregister() })`（`events.ts:254-260`）。

**为什么反转顺序**：后注册的往往依赖先注册的，卸载要反着来（栈语义）。后端类比：Spring 的 Bean 销毁顺序、defer 栈、RAII 析构。

## 五、五种 dispatch 模式

| 模式 | await？ | 顺序 | 有返回值？ |
|---|---|---|---|
| `emit` | 否 | 注册顺序观察 | 否 |
| `waterfall` | 否 | 注册顺序观察 | 是 |
| `parallel` | 是 | 并发观察 | 否 |
| `serial` | 是 | 注册顺序 | 是 |

（Cordis 内部还有 `bail`，同步版 serial。）

**waterfall 是 around-middleware**（`events.ts:234-243`）：listener 收到 `(...args, next)`。调 `next()` 委托到下一个（值经 next 返回值传播）；**不调 `next()` 就短路**，包括内置行为。

**后端类比**：waterfall ≈ 洋葱模型中间件链（Koa/Express 的 `next()`、Java 的 FilterChain、Go 的 `http.Handler` 链）。`next()` 不调 = 短路拦截。

`isBailed`（`events.ts:13`）：返回值非 `null`/`false`/`undefined` 就算 bail——serial/bail 靠这个判断"谁决策了"。

## 六、inject：声明式依赖等待

`ctx.inject(deps, callback)` 是 `ctx.plugin({ inject, apply: callback })` 的简写（`registry.ts:300-302`）。`ctx.plugin()`（`registry.ts:316`）创建 Fiber，fiber **等 inject 声明的服务可用才启动**。

**后端类比**：`inject: ['shell']` ≈ Spring `@Autowired` / 构造器注入，但更强调"依赖顺序由服务需求自动推导"，而不是手动编排 boot 顺序。这是 agent harness 能"任意组合插件"的关键——你挂上 provider，依赖它的 consumer 自动醒来。

## 七、作用域：事件向上流动，绝不向下

`dsh-scope`（`packages/core/scope/src/index.ts`）在 Cordis 之上加了一层作用域路由：

- `ScopeKey`（`index.ts:15`）：不透明、身份比较的作用域键。
- `scopeParents`（`index.ts:39`）：每个 key 的 enclosing scope，一条关系同时支撑两个方向：
  - **注册视图向下继承**：子 scope 看到祖先的 layers。
  - **事件准入向上延伸**：标记为祖先的 listener 收到发给后代 key 的事件。

`scopeTarget`（`index.ts:170-185`）构建 routing-only carrier，filter 检查 scope 链：listener 属于 key 或它的**祖先**时收到事件。**事件向上流，绝不向下**。

**为什么这么设计**：让一个 standing composition（根 scope）能观察到它下面每个 agent 的事件，但 agent 之间互不可见。这就是多 agent 隔离 + 统一观察的机制。

**后端类比**：scope 链 ≈ 树形命名空间 / 父子租户。事件向上流动 ≈ 子节点日志上报父节点，父节点广播不到子节点。

## 八、后端视角速记

| 概念 | 后端等价物 |
|---|---|
| Context 服务仓库 | Spring IoC 容器 |
| isolate / extend | 子容器 / Bean 作用域隔离 |
| Service super(ctx,name) | Bean 注册 + 自动销毁 |
| inject 声明依赖 | @Autowired / 构造器注入（自动推导依赖顺序） |
| effect + disposer 反转 | defer 栈 / RAII / Bean 销毁顺序 |
| waterfall | 洋葱中间件 / FilterChain / http.Handler 链 |
| serial/bail | 责任链（谁先决策谁截断） |
| scope 链 + 事件向上流动 | 父子租户 / 树形命名空间 |

## 九、deepseek-harness 源码对应

| 概念 | 源码位置 |
|---|---|
| Context proxy + 服务仓库 | `vendor/cordis/src/context.ts:42` |
| Context 构造（装 built-in services） | `vendor/cordis/src/context.ts:71-84` |
| extend / isolate / intercept | `vendor/cordis/src/context.ts:99-107, 121-125, 139-145` |
| Service 抽象类 + super(ctx,name) | `vendor/cordis/src/service.ts:11, 42-59` |
| effect（disposer 反转顺序） | `vendor/cordis/src/fiber.ts:418, 431` |
| 五种 dispatch | `vendor/cordis/src/events.ts:183-243` |
| isBailed | `vendor/cordis/src/events.ts:13` |
| inject 简写 + ctx.plugin | `vendor/cordis/src/registry.ts:300-302, 316-336` |
| ScopeKey + scopeParents | `packages/core/scope/src/index.ts:15, 39` |
| bindScopeParent / scopeChainOf | `packages/core/scope/src/index.ts:72, 98` |
| scopeTarget（事件向上流动） | `packages/core/scope/src/index.ts:170-185` |
| Cordis 五个概念 + 五 dispatch 表 | `docs/cordis-primer.md:7-13, 19-24` |

## 十、复现覆盖 + 未复现

**已覆盖**：
- 注册即 effect + disposer 反转顺序 + no-op
- 五种 dispatch（emit / waterfall / serial / bail 语义）
- waterfall 的 next() 委托 + 短路 + 值传播
- inject 依赖声明 + 服务仓库 + 重复注册 fail-loud
- 作用域链（scopeParents）+ 事件向上流动、绝不向下

**未复现（真实实现有、demo 里省略）**：
- Context 的 Proxy 机制（属性读走服务解析器）——Go 无 Proxy，用 map 模拟
- Fiber 的完整生命周期（start/stop/reload、inertia、async teardown）
- `isolate(name, label)` 的同 label 作用域合并
- `intercept` 的 config 合并（ancestor-first）
- `parallel` 的真实并发（Go 里可 goroutine，但 demo 简化）
- declaration merging（TS 类型合并声明事件名/服务名，Go 无对应）
- `ctx.plugin()` 的三种插件形态（function/class/{apply}）归一化
- `internal/*` 框架内部事件（`internal/dispatch` 等诊断）

## 十一、面试要点速记

1. 一切皆插件：function/class/{apply} 三种形态，`ctx.plugin()` 启动，Fiber 管生命周期。
2. Context 是服务仓库 + Proxy，`isolate`/`extend`/`intercept` 创建作用域子 context 不改父。
3. inject 声明依赖，fiber 等依赖可用才启动——加载顺序由服务需求自动推导。
4. 注册即 effect：`ctx.effect()`/`ctx.on()` 返回 disposer，卸载反转顺序跑，可逆。
5. waterfall 是 around-middleware：`next()` 委托、不调短路、值经 next 返回传播。
6. 作用域链让事件向上流动（祖先 listener 收到后代事件）、绝不向下——多 agent 隔离 + 统一观察。
