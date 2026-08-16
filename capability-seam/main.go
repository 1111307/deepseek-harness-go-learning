package main

// deepseek-harness capability-seam 完整复现
// 章节：capability-seam（Service Definition / Service Provider / Consumer 三件套）
//
// 对照真实源码（canonical 例子 = packages/shell 三件套）：
//   shell/shell/src/index.ts        Service Definition（ShellExecutor 抽象类，ctx.shell）
//   shell/bash-local/src/index.ts   Service Provider（LocalBashExecutor extends ShellExecutor）
//   shell/bash-sandbox/src/index.ts Service Provider（SandboxBashExecutor，换实现不动 Consumer）
//   shell/tool-bash/src/index.ts    Consumer（ctx.tools.register(defineTool)，inject ['shell']）
//   .agents/notes/.../capability-seams.md  三角色定义的权威说明
//
// 运行：go run main.go

import (
	"fmt"
	"strings"
)

// ============ Service Definition（dsh-shell/src/index.ts）============

// ShellExecRequest：调用方的执行请求，可选字段（workdir/timeout 缺省）。
// 这是模型/插件面向的形状；交给 Resolve 得到完全解析的 Spec。
type ShellExecRequest struct {
	Command   string
	Workdir   string // 空 = 未指定，由实现填充默认
	TimeoutMs int    // 0 = 未指定，由实现填充默认并 cap
}

// ShellExecSpec：解析后的执行规格。Resolve 填充并 cap 必填字段；Run 收到显式值，
// 绝不再默认（Explicit > implicit at package boundaries）。
type ShellExecSpec struct {
	Command   string
	Workdir   string
	TimeoutMs int
	Mode      string // 解析后的 sandbox policy mode（sandbox provider 才消费）
}

// SandboxInfo：一次运行的 sandbox 事实，独立于退出码，让调用方区分命令失败 vs 策略拒绝。
type SandboxInfo struct {
	Mode        string
	Denied      bool
	Enforcement string
}

// ShellRunResult：一次前台运行的结果。
type ShellRunResult struct {
	ExitCode int
	Stdout   string
	Sandbox  *SandboxInfo // 非 sandbox executor 为 nil
}

// ShellExecutor 是 Service Definition：抽象能力契约。
// 真实里它是 Cordis `Service` 抽象类（super(ctx,'shell') 注册 ctx.shell），
// 绝不是 TS interface——因为要进 ctx 注册表，interface 做不到。
// Go 里用 interface 表达契约语义；"注册进 context"用下面的 Context 模拟。
type ShellExecutor interface {
	// SandboxMode 返回该 executor 默认应用的 sandbox mode，不沙箱时返回 ""。
	SandboxMode() string
	// Resolve 把 request 变成完全指定的 spec（显式默认值步骤）。
	Resolve(req ShellExecRequest) ShellExecSpec
	// Run 前台执行 spec；非零退出/超时/中止 resolve 成结果而非 reject。
	Run(spec ShellExecSpec) ShellRunResult
}

// ============ Context（模拟 Cordis 的 ctx.<key> + 注册即 effect）============

// Context 模拟 Cordis context 的服务注册表。真实里 Provider 通过
// super(ctx, 'shell') 注册，Consumer 通过 inject: ['shell'] 声明依赖（fiber
// pend 直到服务存在）。Go 里用 map + register/get 表达同一语义。
type Context struct {
	services map[string]any
}

func NewContext() *Context {
	return &Context{services: map[string]any{}}
}

// register 模拟"注册即 effect"：一个 key 只能有一个实现，重复注册 fail-loud
// （Cordis 的标准 duplicate-service 行为）。
func (c *Context) register(key string, svc any) error {
	if _, exists := c.services[key]; exists {
		return fmt.Errorf("duplicate service registration: %q is already provided", key)
	}
	c.services[key] = svc
	return nil
}

// get 模拟 Consumer 的注入：按 key 取服务。Consumer 只拿到接口类型，
// 不 import provider 类型。
func (c *Context) get(key string) (any, bool) {
	svc, ok := c.services[key]
	return svc, ok
}

// ============ Service Provider 1：LocalBashExecutor（bash-local/src/index.ts）============

// LocalBashExecutor 是本地 bash 的 Service Provider：extends ShellExecutor，
// 通过 ctx.subprocess 跑 bash -c。它拥有命令默认值、超时 cap、模型友好环境。
type LocalBashExecutor struct {
	cwd         string
	timeoutMs   int // 默认前台超时
	maxTimeoutMs int // 每调用超时上限
}

func NewLocalBashExecutor(cwd string, timeoutMs, maxTimeoutMs int) *LocalBashExecutor {
	return &LocalBashExecutor{cwd: cwd, timeoutMs: timeoutMs, maxTimeoutMs: maxTimeoutMs}
}

func (e *LocalBashExecutor) SandboxMode() string { return "" } // 不沙箱

// Resolve：把 request 变成完全指定的 spec。填充 workdir（config.cwd）、
// timeoutMs（config.timeoutMs，cap 在 maxTimeoutMs）。Run/start 收到显式值，绝不重新默认。
func (e *LocalBashExecutor) Resolve(req ShellExecRequest) ShellExecSpec {
	timeout := req.TimeoutMs
	if timeout == 0 {
		timeout = e.timeoutMs
	}
	if timeout > e.maxTimeoutMs {
		timeout = e.maxTimeoutMs
	}
	workdir := req.Workdir
	if workdir == "" {
		workdir = e.cwd
	}
	return ShellExecSpec{Command: req.Command, Workdir: workdir, TimeoutMs: timeout}
}

// runArgv：用显式 argv 跑（进程生命周期机制）。子类（sandbox）替换 argv 复用此机制。
func (e *LocalBashExecutor) runArgv(spec ShellExecSpec, argv []string) ShellRunResult {
	// 模拟执行：真实是 ctx.subprocess.spawn(argv, {cwd: spec.Workdir, ...}) + 等 done。
	return ShellRunResult{ExitCode: 0, Stdout: fmt.Sprintf("executed argv=%s in %s (timeout=%dms)", strings.Join(argv, " "), spec.Workdir, spec.TimeoutMs)}
}

// Run：前台跑一条 bash -c 命令。
func (e *LocalBashExecutor) Run(spec ShellExecSpec) ShellRunResult {
	return e.runArgv(spec, []string{"bash", "-c", spec.Command})
}

// ============ Service Provider 2：SandboxBashExecutor（bash-sandbox/src/index.ts）============

// 最小 sandbox 桩：只为演示"换 Provider"——真实是 ctx.sandbox 的 bwrap/Landlock/Seatbelt
// 包装 argv（见 sandbox 章）。这里返回包装 argv + enforcement。
type FakeSandbox struct{}

func (s *FakeSandbox) confine(argv []string, mode string) ([]string, string) {
	// ['bash','-c',command] → bwrap 只读挂根包装。mode=workspace-write 时加可写 bind。
	args := []string{"bwrap", "--ro-bind", "/", "/", "--"}
	if mode == "workspace-write" {
		args = append(args, "--bind", "/workspace", "/workspace")
	}
	return append(args, argv...), "full"
}

// SandboxBashExecutor 是 sandbox-consuming 的 Provider：extends LocalBashExecutor，
// 复用本地进程生命周期，只把 argv 换成 ctx.sandbox 包装后的 argv，并附加 sandbox facts。
// 它替换 dsh-bash-local 注册 ctx.shell，tool 层（Consumer）零改动。
type SandboxBashExecutor struct {
	LocalBashExecutor            // 继承 local 的 resolve/runArgv 机制
	mode             string      // sandboxPolicy.defaultMode（真实从 ctx.sandboxPolicy 读）
	sandbox          *FakeSandbox
}

func NewSandboxBashExecutor(local LocalBashExecutor, mode string) *SandboxBashExecutor {
	return &SandboxBashExecutor{LocalBashExecutor: local, mode: mode, sandbox: &FakeSandbox{}}
}

// SandboxMode 覆盖：返回默认 mode（真实是 capability fact，tool 层读它决定是否广告升级字段）。
func (e *SandboxBashExecutor) SandboxMode() string { return e.mode }

// Resolve 覆盖：stamp 默认 sandbox mode（真实是
// `request.sandboxPolicy ?? ctx.sandboxPolicy.resolve()`——工具调用带会话解析的策略，
// 无则回退部署默认）。这是 sandbox provider 相对 local 多出来的一个覆盖点。
func (e *SandboxBashExecutor) Resolve(req ShellExecRequest) ShellExecSpec {
	spec := e.LocalBashExecutor.Resolve(req)
	spec.Mode = e.mode
	return spec
}

// Run 覆盖：danger-full-access 直通；否则 confine 包装 argv，复用 runArgv，附加 sandbox facts。
func (e *SandboxBashExecutor) Run(spec ShellExecSpec) ShellRunResult {
	if spec.Mode == "danger-full-access" {
		r := e.LocalBashExecutor.Run(spec)
		r.Sandbox = &SandboxInfo{Mode: spec.Mode, Denied: false}
		return r
	}
	argv, enforcement := e.sandbox.confine([]string{"bash", "-c", spec.Command}, spec.Mode)
	r := e.runArgv(spec, argv)
	r.Sandbox = &SandboxInfo{Mode: spec.Mode, Denied: false, Enforcement: enforcement}
	return r
}

// ============ Consumer：BashTool（tool-bash/src/index.ts）============

// BashTool 是 model-facing Consumer：inject ['shell']，execute 里
// ctx.shell.resolve(request) 再 ctx.shell.run(spec)。它只依赖 ShellExecutor
// 接口和 Service Definition 的类型，从不 import provider 类型。
type BashTool struct {
	ctx *Context
}

// execute 模拟模型调用 bash 工具：组装 request → resolve → run。
func (t *BashTool) execute(req ShellExecRequest) (ShellRunResult, error) {
	svc, ok := t.ctx.get("shell")
	if !ok {
		return ShellRunResult{}, fmt.Errorf("bash tool: ctx.shell is not provided (no executor mounted)")
	}
	shell := svc.(ShellExecutor) // 注入的是接口，不是具体 provider
	spec := shell.Resolve(req)   // Consumer 永远先 resolve，再 run
	return shell.Run(spec), nil
}

// ============ 演示场景 ============

func scenario1Triple() {
	fmt.Println("=== 场景 1：三件套结构 + resolve 显式默认值 ===\n")

	ctx := NewContext()
	// Provider 注册 ctx.shell（真实是 super(ctx,'shell')）
	if err := ctx.register("shell", NewLocalBashExecutor("/workspace", 120000, 600000)); err != nil {
		fmt.Println("  register:", err)
	}
	// Consumer 注入 ctx.shell
	tool := &BashTool{ctx: ctx}

	// 模型调用，request 不带 workdir/timeout → resolve 填充默认值。
	r1, _ := tool.execute(ShellExecRequest{Command: "ls -la"})
	fmt.Printf("  request(命令, 无 workdir/timeout) → %s\n", r1.Stdout)

	// 直接展示 resolve 的两个步骤：cap 和 default。
	local := NewLocalBashExecutor("/workspace", 120000, 600000)
	spec := local.Resolve(ShellExecRequest{Command: "x", TimeoutMs: 900000})
	fmt.Printf("  resolve cap: request.timeoutMs=900000 → spec.timeoutMs=%d（上限 600000）\n", spec.TimeoutMs)
	spec2 := local.Resolve(ShellExecRequest{Command: "x"})
	fmt.Printf("  resolve default: request 无 timeout → spec.timeoutMs=%d（默认 120000）\n", spec2.TimeoutMs)
	spec3 := local.Resolve(ShellExecRequest{Command: "x", Workdir: "/custom"})
	fmt.Printf("  resolve default: request 无 workdir → spec.Workdir=%q（默认 /workspace）；带 workdir=%q 时尊重\n", spec2.Workdir, spec3.Workdir)
}

func scenario2SwapProvider() {
	fmt.Println("\n=== 场景 2：换 Provider（local → sandbox）不动 Consumer ===\n")

	// 同一个 Consumer 代码，换挂载的 provider。
	run := func(tool *BashTool, label string) {
		r, err := tool.execute(ShellExecRequest{Command: "rm -rf /tmp/x"})
		if err != nil {
			fmt.Printf("  [%s] %v\n", label, err)
			return
		}
		sandbox := "无"
		if r.Sandbox != nil {
			sandbox = fmt.Sprintf("mode=%s enforcement=%s denied=%v", r.Sandbox.Mode, r.Sandbox.Enforcement, r.Sandbox.Denied)
		}
		fmt.Printf("  [%s] %s | sandbox=%s\n", label, r.Stdout, sandbox)
	}

	// 挂 local provider。
	ctx1 := NewContext()
	ctx1.register("shell", NewLocalBashExecutor("/workspace", 120000, 600000))
	run(&BashTool{ctx: ctx1}, "local  executor")

	// 挂 sandbox provider（替换 local），Consumer 零改动。
	ctx2 := NewContext()
	local := NewLocalBashExecutor("/workspace", 120000, 600000)
	ctx2.register("shell", NewSandboxBashExecutor(*local, "workspace-write"))
	run(&BashTool{ctx: ctx2}, "sandbox executor")
}

func scenario3Registration() {
	fmt.Println("\n=== 场景 3：注册即 effect + Consumer 只依赖接口 ===\n")

	ctx := NewContext()
	ctx.register("shell", NewLocalBashExecutor("/workspace", 120000, 600000))
	// 重复注册第二个实现 → fail-loud（Cordis 的 duplicate-service）。
	if err := ctx.register("shell", NewLocalBashExecutor("/other", 1, 1)); err != nil {
		fmt.Printf("  重复注册第二个 shell provider → %v\n", err)
	}

	// Consumer 注入的是接口类型（ShellExecutor），代码里没有 LocalBashExecutor
	// 或 SandboxBashExecutor 的出现——只 import Service Definition 的类型。
	tool := &BashTool{ctx: ctx}
	if r, err := tool.execute(ShellExecRequest{Command: "echo hi"}); err == nil {
		fmt.Printf("  Consumer 注入接口，跑通 → %s\n", r.Stdout)
	}

	// 无 provider 时 Consumer 报错（inject 依赖未满足）。
	empty := NewContext()
	if _, err := (&BashTool{ctx: empty}).execute(ShellExecRequest{Command: "echo hi"}); err != nil {
		fmt.Printf("  无 provider 时 Consumer → %v\n", err)
	}
}

func main() {
	fmt.Println("=== capability-seam：Service Definition / Service Provider / Consumer ===\n")
	scenario1Triple()
	scenario2SwapProvider()
	scenario3Registration()

	fmt.Println("\n=== 关键点 ===")
	fmt.Println("1. Service Definition 是 Cordis Service 抽象类（拥有 ctx.<key> + 词汇类型），绝不是 TS interface")
	fmt.Println("2. Provider extends Service Definition，super(ctx,'shell') 注册；一个 key 一个实现，重复 fail-loud")
	fmt.Println("3. Consumer inject ['shell']，只依赖接口类型，从不 import provider 类型")
	fmt.Println("4. resolve(request): Spec 是显式默认值步骤——Run 收显式值，绝不重新默认")
	fmt.Println("5. 换 Provider 不动 Consumer：sandbox 替换 local，tool schema 零改动")
}
