package main

// deepseek-harness plugin-runtime 完整复现
// 章节：plugin-runtime（Cordis 插件化 + 注册即 effect + 作用域上下文）
//
// 对照真实源码（vendored Cordis）：
//   vendor/cordis/src/context.ts    Context proxy：服务仓库 + extend/isolate/intercept 作用域
//   vendor/cordis/src/service.ts    Service 抽象类：super(ctx,name) 注册，fiber 卸载自动移除
//   vendor/cordis/src/fiber.ts      effect：立即执行 + disposer 反转顺序卸载
//   vendor/cordis/src/events.ts     五种 dispatch：emit/parallel/serial/bail/waterfall
//   vendor/cordis/src/registry.ts   inject 依赖声明 + ctx.plugin 启动 fiber
//   packages/core/scope/src/index.ts  ScopeKey + scopeParents + scopeTarget（事件向上流动）
//
// 运行：go run main.go

import (
	"fmt"
	"sync"
)

// ============ Context：服务仓库 + 事件总线 + effect ============

// WaterfallListener 是 waterfall 事件的监听器：收到 next 委托函数，
// 调 next() 委托到下一个（值经 next 返回传播），不调 next 则短路。
type WaterfallListener func(next func() any) any

// ObserverListener 是观察型事件（emit/serial/bail）的监听器：返回 bail 值。
type ObserverListener func() any

// Context 模拟 Cordis context：服务仓库（ctx.<key>）+ 事件总线 + effect 栈。
type Context struct {
	services map[string]any
	wfHooks  map[string][]WaterfallListener
	obsHooks map[string][]ObserverListener
}

func NewContext() *Context {
	return &Context{
		services: map[string]any{},
		wfHooks:  map[string][]WaterfallListener{},
		obsHooks: map[string][]ObserverListener{},
	}
}

// provide 注册服务到 ctx.<key>（真实是 super(ctx,name) → reflect.provide）。
// 一个 key 一个实现，重复 fail-loud。
func (c *Context) provide(name string, svc any) error {
	if _, exists := c.services[name]; exists {
		return fmt.Errorf("duplicate service registration: %q is already provided", name)
	}
	c.services[name] = svc
	return nil
}

func (c *Context) get(name string) (any, bool) {
	svc, ok := c.services[name]
	return svc, ok
}

// effect 对应 ctx.effect()：body 立即执行，body 里通过 collect 注册的 disposer
// 被收集；返回的 disposer 调用时按【反转顺序】跑（对应 fiber.ts:431 的 reverse）。
// 重复调用 disposer 是 no-op（sync.Once）。
func (c *Context) effect(body func(collect func(dispose func()))) func() {
	var disposables []func()
	body(func(dispose func()) { disposables = append(disposables, dispose) })
	var once sync.Once
	return func() {
		once.Do(func() {
			for i := len(disposables) - 1; i >= 0; i-- {
				disposables[i]()
			}
		})
	}
}

// onWaterfall 注册 waterfall 监听器（真实是 ctx.on，本质 ctx.fiber.effect）。
// 返回 disposer 从 hooks 移除监听器。
func (c *Context) onWaterfall(name string, fn WaterfallListener) func() {
	c.wfHooks[name] = append(c.wfHooks[name], fn)
	return func() {
		for i, h := range c.wfHooks[name] {
			if &h == &fn {
				c.wfHooks[name] = append(c.wfHooks[name][:i], c.wfHooks[name][i+1:]...)
				return
			}
		}
	}
}

// onObserve 注册观察型监听器。
func (c *Context) onObserve(name string, fn ObserverListener) func() {
	c.obsHooks[name] = append(c.obsHooks[name], fn)
	return func() {
		for i, h := range c.obsHooks[name] {
			if &h == &fn {
				c.obsHooks[name] = append(c.obsHooks[name][:i], c.obsHooks[name][i+1:]...)
				return
			}
		}
	}
}

// emit：同步跑监听器，不 await，忽略返回值（events.ts:194）。
func (c *Context) emit(name string) {
	for _, h := range c.obsHooks[name] {
		h()
	}
}

// waterfall：组合监听器。listener 最外层先跑，调 next() 委托，不调 next 短路；
// 值经 next() 返回值传播（events.ts:234-243）。
func (c *Context) waterfall(name string, inner func() any) any {
	cbs := append([]WaterfallListener{}, c.wfHooks[name]...) // 快照
	var next func() any
	next = func() any {
		if len(cbs) > 0 {
			cb := cbs[0]
			cbs = cbs[1:]
			return cb(next)
		}
		return inner() // 内置行为
	}
	return next()
}

// serial：顺序 await 直到一个返回 bail 值（非 nil/false）（events.ts:204）。
func (c *Context) serial(name string) any {
	for _, h := range c.obsHooks[name] {
		if r := h(); r != nil && r != false {
			return r
		}
	}
	return nil
}

// bail：同步顺序直到一个返回 bail 值（events.ts:217）。
func (c *Context) bail(name string) any {
	for _, h := range c.obsHooks[name] {
		if r := h(); r != nil && r != false {
			return r
		}
	}
	return nil
}

// ============ 作用域（dsh-scope/src/index.ts）============

// ScopeKey 是不透明、身份比较的作用域键。
type ScopeKey struct{ id string }

// scopeParents 记录每个 key 的 enclosing scope（scopeParents WeakMap）。
var scopeParents = map[*ScopeKey]*ScopeKey{}

func bindScopeParent(key, parent *ScopeKey) { scopeParents[key] = parent }

// scopeChainOf：从 key 到根，nearest-first。
func scopeChainOf(key *ScopeKey) []*ScopeKey {
	var chain []*ScopeKey
	for k := key; k != nil; k = scopeParents[k] {
		chain = append(chain, k)
	}
	return chain
}

// scopeAdmits 对应 scopeTarget 的 filter：listener 属于 key 或它的祖先时，
// 收到发给 dispatchKey 的事件——事件向上流动，绝不向下（scope/index.ts:170-185）。
func scopeAdmits(listenerKey, dispatchKey *ScopeKey) bool {
	if dispatchKey == nil {
		return true // 无 tag → 全局接收
	}
	for k := dispatchKey; k != nil; k = scopeParents[k] {
		if k == listenerKey {
			return true
		}
	}
	return false
}

// ============ 演示场景 ============

func scenario1Effect() {
	fmt.Println("=== 场景 1：注册即 effect + disposer 反转顺序 ===\n")

	ctx := NewContext()
	// 一个插件挂载后做的注册：effect 里注册 A、B 两个 disposer。
	dispose := ctx.effect(func(collect func(func())) {
		collect(func() { fmt.Println("    卸载 disposer A（后注册，先跑）") })
		collect(func() { fmt.Println("    卸载 disposer B（先注册，后跑）") })
		fmt.Println("    effect body 执行：注册了 A、B 两个 disposer")
	})

	fmt.Println("  调用 disposer：")
	dispose()
	fmt.Println("  再次调用 disposer：no-op（sync.Once）")
	dispose()
}

func scenario2Dispatch() {
	fmt.Println("\n=== 场景 2：五种 dispatch 模式（waterfall 重点）===\n")

	ctx := NewContext()

	// emit：同步观察，忽略返回值。
	ctx.onObserve("greet", func() any { fmt.Println("    emit 观察者 1"); return nil })
	ctx.onObserve("greet", func() any { fmt.Println("    emit 观察者 2"); return nil })
	fmt.Println("  emit('greet')：")
	ctx.emit("greet")

	// waterfall：中间件链。listener 调 next 委托，不调短路。
	ctx.onWaterfall("request", func(next func() any) any {
		fmt.Println("    中间件 A 进入")
		val := next() // 委托
		fmt.Println("    中间件 A 退出，收到", val)
		return "A 包装: " + val.(string)
	})
	ctx.onWaterfall("request", func(next func() any) any {
		fmt.Println("    中间件 B 进入（短路，不调 next）")
		return "B 短路值"
	})
	fmt.Println("  waterfall('request')：")
	result := ctx.waterfall("request", func() any { return "内置行为" })
	fmt.Println("    最终结果:", result)

	// serial：顺序 await 直到 bail。
	ctx2 := NewContext()
	ctx2.onObserve("decide", func() any { fmt.Println("    serial 监听器 1（无决策）"); return nil })
	ctx2.onObserve("decide", func() any { fmt.Println("    serial 监听器 2（决策=bail）"); return "decision" })
	ctx2.onObserve("decide", func() any { fmt.Println("    serial 监听器 3（不会跑）"); return nil })
	fmt.Println("  serial('decide')：")
	fmt.Println("    第一个 bail 值:", ctx2.serial("decide"))
}

func scenario3Inject() {
	fmt.Println("\n=== 场景 3：inject 依赖声明 + 服务仓库 ===\n")

	ctx := NewContext()

	// 模拟一个声明 inject: ['shell'] 的插件：只有 shell 服务可用才启动。
	// 真实是 fiber pend 等依赖；Go 里简化成检查依赖是否已注册。
	startPlugin := func(label string, inject []string) {
		missing := false
		for _, dep := range inject {
			if _, ok := ctx.get(dep); !ok {
				fmt.Printf("  [%s] 等待依赖 %q（尚未注册，fiber pend）\n", label, dep)
				missing = true
			}
		}
		if !missing {
			fmt.Printf("  [%s] 依赖满足，插件启动\n", label)
		}
	}

	fmt.Println("  挂 bash 工具插件（inject ['shell']）：")
	startPlugin("tool-bash", []string{"shell"})

	fmt.Println("  注册 shell 服务（Provider 挂载）：")
	ctx.provide("shell", "LocalBashExecutor")
	startPlugin("tool-bash", []string{"shell"})

	fmt.Println("  按 key 取服务，不 import 具体实现：")
	if svc, ok := ctx.get("shell"); ok {
		fmt.Printf("    ctx.get(%q) = %v\n", "shell", svc)
	}

	// 重复注册 fail-loud。
	if err := ctx.provide("shell", "SandboxBashExecutor"); err != nil {
		fmt.Printf("  重复注册第二个 shell 实现 → %v\n", err)
	}
}

func scenario4Scope() {
	fmt.Println("\n=== 场景 4：作用域链 + 事件向上流动 ===\n")

	// 根 composition scope R，下面两个 agent scope A1、A2。
	R := &ScopeKey{id: "root"}
	A1 := &ScopeKey{id: "agent-1"}
	A2 := &ScopeKey{id: "agent-2"}
	bindScopeParent(A1, R)
	bindScopeParent(A2, R)

	fmt.Printf("  scope 链：A1 → %v；A2 → %v\n",
		keysOf(scopeChainOf(A1)), keysOf(scopeChainOf(A2)))

	// 根 scope 的 standing listener：能收到每个后代 agent 的事件（向上流动）。
	fmt.Println("  根 scope 的 listener 观察各 agent 的事件：")
	fmt.Printf("    listener@R 收到 agent-1 事件? %v\n", scopeAdmits(R, A1))
	fmt.Printf("    listener@R 收到 agent-2 事件? %v\n", scopeAdmits(R, A2))

	// agent 的 listener：收不到兄弟或父 scope 的事件（不向下）。
	fmt.Println("  agent 的 listener 收不到父/兄弟 scope 事件：")
	fmt.Printf("    listener@A1 收到 root 事件? %v\n", scopeAdmits(A1, R))
	fmt.Printf("    listener@A1 收到 agent-2 事件? %v\n", scopeAdmits(A1, A2))
	fmt.Printf("    listener@A1 收到自己的事件? %v\n", scopeAdmits(A1, A1))

	// 无 tag（dispatchKey=nil）→ 全局接收。
	fmt.Printf("    listener@A1 收到无 tag 全局事件? %v\n", scopeAdmits(A1, nil))
}

func keysOf(keys []*ScopeKey) string {
	s := "["
	for i, k := range keys {
		if i > 0 {
			s += ", "
		}
		s += k.id
	}
	return s + "]"
}

func main() {
	fmt.Println("=== plugin-runtime：一切皆插件 + 注册即 effect + 作用域 ===\n")
	scenario1Effect()
	scenario2Dispatch()
	scenario3Inject()
	scenario4Scope()

	fmt.Println("\n=== 关键点 ===")
	fmt.Println("1. 一切皆插件：function/class/{apply}，ctx.plugin() 启动，生命周期由 Fiber 管理")
	fmt.Println("2. Context 是服务仓库：ctx.<key> 存服务，按 key 找，不 import 具体实现")
	fmt.Println("3. inject 声明依赖：加载顺序通过服务需求表达，不是手动 boot 顺序")
	fmt.Println("4. 注册即 effect：ctx.effect()/ctx.on() 返回 disposer，卸载反转顺序跑（可逆）")
	fmt.Println("5. waterfall：next() 委托，不调 next 短路，值经 next 返回传播")
	fmt.Println("6. 作用域：事件向上流动（祖先 listener 收到后代事件），绝不向下")
}
