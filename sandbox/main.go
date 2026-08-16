package main

// deepseek-harness sandbox 完整复现
// 章节：sandbox
//
// 对照真实源码：
//   sandbox/sandbox/src/index.ts            Service Definition（SandboxProvider + SandboxMode + ConfinedArgv）
//   sandbox/sandbox-local/src/index.ts      平台 runner 链选择 + confine 包装 argv + fail-closed
//   sandbox/sandbox-local/src/profiles.ts   bwrap/landlock/seatbelt 三种 profile 构造
//   sandbox/sandbox/src/roots.ts            writableRoots（workspace-write 的可写根集合）
//   sandbox/sandbox/src/escalation.ts       WIDER_MODES 阶梯 + approveEscalation fail-closed 序列
//   sandbox/sandbox-policy/src/index.ts     策略解析（部署默认 → 会话覆盖 → 显式覆盖）
//   sandbox/sandbox-policy/src/session-mode.ts  effectiveSandboxMode（日志 fold）
//   fs/fs-sandbox/src/containment.ts        isPathUnder（词法快路径 + 文件系统身份回退）
//   fs/fs-sandbox/src/index.ts              checkedTarget（canonicalize-then-contain 进程内栅栏）
//
// 运行：go run main.go

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ============ 模式与策略 ============

type SandboxMode string

const (
	ReadOnly         SandboxMode = "read-only"
	WorkspaceWrite   SandboxMode = "workspace-write"
	DangerFullAccess SandboxMode = "danger-full-access"
)

// ConfinedSandboxMode = ReadOnly | WorkspaceWrite（危险全访问不算"被约束"）
func isConfined(m SandboxMode) bool { return m == ReadOnly || m == WorkspaceWrite }

// SandboxExecutionPolicy：一次能力调用解析出的完整文件效应策略。
// root 即使在不消费它的模式下也携带，让调用方先解析策略、再选执行路径。
type SandboxExecutionPolicy struct {
	Mode          SandboxMode
	WorkspaceRoot string
	SessionID     string // 不透明会话标识（真实是 branded SessionId）
}

type SandboxEnforcement string

const (
	EnforcementFull    SandboxEnforcement = "full"
	EnforcementPartial SandboxEnforcement = "partial"
)

// ============ 平台 runner 链 ============

type Runner string

const (
	RunnerBwrap       Runner = "bwrap"
	RunnerLandlock    Runner = "landlock"
	RunnerSeatbelt    Runner = "seatbelt"
	RunnerWindowsAcl  Runner = "windows-acl"
)

// PLATFORM_CHAINS：先按平台选链，再按探针仲裁。
// linux 有 bwrap/landlock 两个候选（需探针仲裁）；darwin/win32 唯一候选，不探针。
var PlatformChains = map[string][]Runner{
	"linux":  {RunnerBwrap, RunnerLandlock},
	"darwin": {RunnerSeatbelt},
	"win32":  {RunnerWindowsAcl},
}

// STATIC_ENFORCEMENT：不探针直接选中时声明的完整度。
// bwrap/seatbelt 的 profile 按构造就能管住每个承诺的文件效应；windows-acl 因
// Everyone 必须留在限制列表 + NTFS 硬链接别名，只能承诺 partial。
var StaticEnforcement = map[Runner]SandboxEnforcement{
	RunnerBwrap:      EnforcementFull,
	RunnerLandlock:   EnforcementFull,
	RunnerSeatbelt:   EnforcementFull,
	RunnerWindowsAcl: EnforcementPartial,
}

// DENIAL_SIGNATURES：每个 runner 的内核"拒绝方言"——被拒绝的文件效应在 stderr 上
// 产生的（大小写不敏感的）子串。消费者据此推断"是拒绝，不是命令失败"。
var DenialSignatures = map[Runner][]string{
	RunnerBwrap:      {"read-only file system"},
	RunnerLandlock:   {"permission denied"},
	RunnerSeatbelt:   {"operation not permitted"},
	RunnerWindowsAcl: {"access is denied", "access to the path", "permission denied"},
}

type RunnerFailureRule struct {
	AllowedExitCodes    []int
	FatalSignatures     []string
	InformationalLines  []string
}

// RUNNER_FAILURE_RULES：runner 自身的致命诊断。
// 关键区别：runner failure = 命令根本没跑；denial = 约束生效、挡住了命令。
var RunnerFailureRules = map[Runner][]RunnerFailureRule{
	RunnerBwrap:    {{FatalSignatures: []string{"bwrap: "}}},
	RunnerLandlock: {{
		AllowedExitCodes:   []int{125}, // LAUNCHER_FAILURE_EXIT
		FatalSignatures:    []string{"landlock-run: "},
		InformationalLines: []string{"landlock-run: partial enforcement (older Landlock ABI)"},
	}},
	RunnerSeatbelt:   {{FatalSignatures: []string{"sandbox-exec: "}}},
	RunnerWindowsAcl: {{AllowedExitCodes: []int{127}, FatalSignatures: []string{"windows-acl-run: "}}},
}

// ConfinedArgv：confine 的结果——替代调用方原 argv 去 spawn 的包装 argv + 执行完整度
// + 拒绝方言 + runner 失败规则。
type ConfinedArgv struct {
	Argv               []string
	Enforcement        SandboxEnforcement
	DenialSignatures   []string
	RunnerFailureRules []RunnerFailureRule
}

// ============ canonicalPath + writableRoots（roots.ts）============

// canonicalPath：把授予根解析成执行层真正比较的路径（symlink 已解析）。
// 解析失败返回原样——缺失的根匹配不到任何东西，直到它存在（保守结果）。
func canonicalPath(path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return real
}

// writableRoots：一次约束执行可以写的根集合——mode 语义作为 canonical、去重的
// allow-list 的唯一归属地。read-only 允许写空集；workspace-write 允许 workspace
// root、/tmp、os 临时目录（mkstemp 家族的真实临时区）。
func writableRoots(policy SandboxExecutionPolicy) []string {
	if policy.Mode != WorkspaceWrite {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range []string{policy.WorkspaceRoot, "/tmp", os.TempDir()} {
		c := canonicalPath(p)
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// ============ 三种 profile 构造（profiles.ts）============

// bwrap：只读挂根 + dev/proc + die-with-parent；workspace-write 再加 tmpfs /tmp + 绑定 workspace。
func bwrapProfileArgs(policy SandboxExecutionPolicy) []string {
	args := []string{"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--die-with-parent"}
	if policy.Mode == WorkspaceWrite {
		args = append(args, "--tmpfs", "/tmp")
		args = append(args, "--bind", policy.WorkspaceRoot, policy.WorkspaceRoot)
	}
	return args
}

// landlock：把策略表达成 launcher 的 allow-list grant。
// 真实 grant 参数由 @deepseek-ai/node-addon-landlock-run 的 landlockGrantArgs 生成；
// 这里保留语义（readOnly=/，readWrite 含 /dev/null，workspace-write 再加 /tmp + workspace）。
func landlockProfileArgs(policy SandboxExecutionPolicy) []string {
	readWrite := []string{"/dev/null"}
	if policy.Mode == WorkspaceWrite {
		readWrite = append(readWrite, "/tmp", policy.WorkspaceRoot)
	}
	return landlockGrantArgs("/", readWrite)
}

// landlockGrantArgs：launcher 的 grant 参数（近似形式，真实拼写由 launcher 拥有）。
func landlockGrantArgs(readOnly string, readWrite []string) []string {
	return []string{"--read-only", readOnly, "--read-write", strings.Join(readWrite, ",")}
}

// seatbelt：SBPL profile。可写根来自共享的 writableRoots，保证 Seatbelt grant 和
// 进程内 fs 栅栏永不漂移。
func seatbeltProfileArgs(policy SandboxExecutionPolicy) []string {
	forms := []string{"(version 1)", "(allow default)", "(deny file-write*)", `(allow file-write* (literal "/dev/null"))`}
	roots := writableRoots(policy)
	if len(roots) > 0 {
		subs := make([]string, 0, len(roots))
		for _, r := range roots {
			subs = append(subs, fmt.Sprintf("(subpath %q)", r))
		}
		forms = append(forms, fmt.Sprintf("(allow file-write* %s)", strings.Join(subs, " ")))
	}
	return []string{"-p", strings.Join(forms, " ")}
}

// ============ Provider：runner 链选择 + confine（sandbox-local/index.ts）============

type SelectedRunner struct {
	Runner      Runner
	Enforcement SandboxEnforcement
}

type Provider struct {
	platform       string
	probeBwrap     func() bool
	probeLandlock  func() (SandboxEnforcement, bool)
	probeSeatbelt  func() bool
	probeWindowsAcl func() bool
	selected       *SelectedRunner // 缓存链裁决，终身一次
}

func NewProvider(platform string) *Provider {
	return &Provider{
		platform: platform,
		probeBwrap: func() bool {
			// 真实是 spawnSync bwrap --ro-bind / / --dev /dev --proc /proc --die-with-parent -- true
			return false // 默认不可用，场景里注入
		},
		probeLandlock: func() (SandboxEnforcement, bool) {
			// 真实是 launcher 功能探针：能构建并执行 ruleset → full/partial，否则 unusable
			return EnforcementFull, false
		},
		probeSeatbelt: func() bool {
			return false
		},
		probeWindowsAcl: func() bool {
			return false
		},
	}
}

// confine：把 argv 包进所选 runner 的调用，返回替代 argv + 完整度 + 拒绝方言 + 失败规则。
// 平台无可用 runner 时抛 SANDBOX_UNAVAILABLE（fail-closed），绝不返回原 argv。
func (p *Provider) confine(argv []string, policy SandboxExecutionPolicy) (ConfinedArgv, error) {
	selected, err := p.selectRunner()
	if err != nil {
		return ConfinedArgv{}, err
	}
	runnerArgv := p.runnerArgv(selected.Runner, policy)
	wrapped := append(append([]string{}, runnerArgv...), append([]string{"--"}, argv...)...)
	return ConfinedArgv{
		Argv:               wrapped,
		Enforcement:        selected.Enforcement,
		DenialSignatures:   DenialSignatures[selected.Runner],
		RunnerFailureRules: RunnerFailureRules[selected.Runner],
	}, nil
}

func (p *Provider) runnerArgv(r Runner, policy SandboxExecutionPolicy) []string {
	switch r {
	case RunnerBwrap:
		return append([]string{"bwrap"}, bwrapProfileArgs(policy)...)
	case RunnerLandlock:
		return append([]string{"landlock-run"}, landlockProfileArgs(policy)...)
	case RunnerSeatbelt:
		return append([]string{"sandbox-exec"}, seatbeltProfileArgs(policy)...)
	case RunnerWindowsAcl:
		return []string{"node", "runner.js", "--workspace", policy.WorkspaceRoot, "--mode", string(policy.Mode)}
	default:
		panic("unreachable runner")
	}
}

func (p *Provider) selectRunner() (SelectedRunner, error) {
	if p.selected != nil {
		return *p.selected, nil
	}
	chain := PlatformChains[p.platform]
	if len(chain) == 0 {
		return SelectedRunner{}, fmt.Errorf("sandbox mode requested but no sandbox backend is usable on this host (SANDBOX_UNAVAILABLE)")
	}
	// 唯一候选：不需要仲裁，直接选；执行时拒绝仍 fail-closed。
	if len(chain) == 1 {
		r := SelectedRunner{chain[0], StaticEnforcement[chain[0]]}
		p.selected = &r
		return r, nil
	}
	// 多候选：按链顺序探针仲裁，第一个可用者胜出。
	for _, runner := range chain {
		if en, ok := p.probeRunner(runner); ok {
			r := SelectedRunner{runner, en}
			p.selected = &r
			return r, nil
		}
	}
	return SelectedRunner{}, fmt.Errorf("sandbox mode requested but no sandbox backend is usable on this host (SANDBOX_UNAVAILABLE)")
}

func (p *Provider) probeRunner(r Runner) (SandboxEnforcement, bool) {
	switch r {
	case RunnerBwrap:
		return EnforcementFull, p.probeBwrap()
	case RunnerLandlock:
		return p.probeLandlock()
	case RunnerSeatbelt:
		return EnforcementFull, p.probeSeatbelt()
	case RunnerWindowsAcl:
		return EnforcementPartial, p.probeWindowsAcl()
	default:
		panic("unreachable runner")
	}
}

// ============ 进程内 fs 栅栏（fs-sandbox/containment.ts + index.ts）============

// isLexicallyUnder：词法快路径——target 是 root 或它的后代。
func isLexicallyUnder(path, root string, caseSensitive bool) bool {
	if !caseSensitive {
		path, root = strings.ToLower(path), strings.ToLower(root)
	}
	if path == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(path, prefix)
}

// isPathUnder：判断 target 是否可写根或其下。词法快路径处理常规 canonical 拼写；
// 拼写不同时，向上遍历已存在祖先并比较文件系统身份（dev+ino），识别 Windows
// 长名/8.3 别名和大小写，而不把 containment 弱化成文本近似。
func isPathUnder(path, root string, caseSensitive bool) bool {
	if isLexicallyUnder(path, root, caseSensitive) {
		return true
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false // 根缺失 → 匹配不到
	}
	ancestor := path
	for {
		info, err := os.Stat(ancestor)
		if err == nil && os.SameFile(info, rootInfo) {
			return true
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return false
		}
		ancestor = parent
	}
}

// checkedTarget：per-call 策略栅栏——read-only 拒绝；workspace-write 现在重新
// canonicalize（反映并发换掉的 symlink）、要求包含在可写根内、返回那个 FRESH target
// （不 check 这里、写那里，避免 TOCTOU）；danger-full-access 无栅栏直通。
func checkedTarget(target string, policy SandboxExecutionPolicy) (string, error) {
	switch policy.Mode {
	case DangerFullAccess:
		return target, nil
	case ReadOnly:
		return "", fmt.Errorf("cannot write %q: file access denied under read-only mode (FS_SANDBOX_DENIED)", target)
	case WorkspaceWrite:
		fresh := canonicalPath(target)
		for _, root := range writableRoots(policy) {
			if isPathUnder(fresh, root, true) {
				return fresh, nil
			}
		}
		return "", fmt.Errorf("cannot write %q: file access denied under workspace-write mode (FS_SANDBOX_DENIED)", target)
	default:
		panic("unreachable mode")
	}
}

// ============ 升级（escalation.ts）============

// WIDER_MODES：严格更宽表——key 是调用当前有效模式，value 是可升级到的模式。
var WiderModes = map[SandboxMode][]SandboxMode{
	ReadOnly:       {WorkspaceWrite, DangerFullAccess},
	WorkspaceWrite: {DangerFullAccess},
}

type EscalationOutcome string

const (
	OutcomeAllowedOnce EscalationOutcome = "allowed-once"
	OutcomeRejected    EscalationOutcome = "rejected"
	OutcomeCancelled   EscalationOutcome = "cancelled"
	OutcomeUnavailable EscalationOutcome = "unavailable"
)

// approveEscalation：在任何执行前裁决升级请求——先校验严格更宽，再走审批通道，
// 再映射每个结果。非更宽请求绝不打扰人；missing approver/agent、rejected、
// cancelled、unanswerable 各自抛不同文案，工具层把它变成 isError 结果，什么都没跑。
func approveEscalation(requestedMode, effectiveMode SandboxMode, justification, subject string, approver func() EscalationOutcome, hasAgent bool) (SandboxMode, error) {
	wider := WiderModes[effectiveMode]
	if !contains(wider, requestedMode) {
		return "", fmt.Errorf("sandbox escalation to %q is not strictly wider than this call's current %q mode", requestedMode, effectiveMode)
	}
	if approver == nil {
		return "", fmt.Errorf("sandbox escalation to %q requires approval, but no approval service is composed", requestedMode)
	}
	if !hasAgent {
		return "", fmt.Errorf("sandbox escalation to %q requires approval, but the call has no agent to route it through", requestedMode)
	}
	outcome := approver()
	switch outcome {
	case OutcomeAllowedOnce:
		return requestedMode, nil
	case OutcomeRejected:
		return "", fmt.Errorf("the user rejected escalating this %s to %q", subject, requestedMode)
	case OutcomeCancelled:
		return "", fmt.Errorf("approval for escalating to %q was cancelled", requestedMode)
	case OutcomeUnavailable:
		return "", fmt.Errorf("sandbox escalation to %q requires approval, but no approval channel is available", requestedMode)
	default:
		panic("unreachable outcome")
	}
}

func contains(modes []SandboxMode, m SandboxMode) bool {
	for _, x := range modes {
		if x == m {
			return true
		}
	}
	return false
}

// ============ 策略解析（sandbox-policy + session-mode）============

type Event struct {
	Type string
	Data map[string]interface{}
}

type Session struct {
	Events []Event
	Cwd    string
}

// effectiveSandboxMode：会话的 sandbox 覆盖——日志里最后一个 sandbox/mode 事件，
// 或 undefined（无覆盖时应用部署默认）。纯 fold：重放日志就是状态。
func effectiveSandboxMode(events []Event) *SandboxMode {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "sandbox/mode" {
			m := events[i].Data["mode"].(SandboxMode)
			return &m
		}
	}
	return nil
}

type SandboxPolicyService struct {
	defaultMode   SandboxMode
	workspaceRoot string
}

// resolve：为一次能力调用解析完整策略。优先级：显式覆盖 > 会话最后一个 sandbox/mode
// 事件 > 部署默认。会话 cwd 是 workspace-write 边界；配置根是 agentless 调用的回退。
func (s *SandboxPolicyService) resolve(session *Session, explicitMode *SandboxMode) SandboxExecutionPolicy {
	mode := s.defaultMode
	if session != nil {
		if ov := effectiveSandboxMode(session.Events); ov != nil {
			mode = *ov
		}
	}
	if explicitMode != nil {
		mode = *explicitMode
	}
	wsRoot := s.workspaceRoot
	if session != nil && session.Cwd != "" {
		wsRoot = session.Cwd
	}
	return SandboxExecutionPolicy{Mode: mode, WorkspaceRoot: canonicalPath(wsRoot), SessionID: sessionID(session)}
}

func sessionID(s *Session) string {
	if s == nil {
		return ""
	}
	return "sess-1"
}

// ============ 演示场景 ============

func scenario1PlatformChain() {
	fmt.Println("=== 场景 1：平台 runner 链选择 + fail-closed ===\n")

	// linux：两个候选，探针仲裁。先 bwrap 可用 → 选 bwrap。
	lin := NewProvider("linux")
	lin.probeBwrap = func() bool { return true }
	lin.probeLandlock = func() (SandboxEnforcement, bool) { return EnforcementFull, true }
	c, _ := lin.confine([]string{"bash", "-c", "echo hi"}, SandboxExecutionPolicy{Mode: ReadOnly, WorkspaceRoot: "/ws"})
	fmt.Printf("  linux（bwrap 可用）→ runner=%s\n", c.Argv[0])
	fmt.Printf("    argv=%v\n", c.Argv)
	fmt.Printf("    enforcement=%s  denial=%v\n", c.Enforcement, c.DenialSignatures)

	// bwrap 不可用，landlock 可用 → 选 landlock。
	lin2 := NewProvider("linux")
	lin2.probeBwrap = func() bool { return false }
	lin2.probeLandlock = func() (SandboxEnforcement, bool) { return EnforcementFull, true }
	c2, _ := lin2.confine([]string{"bash", "-c", "echo hi"}, SandboxExecutionPolicy{Mode: ReadOnly, WorkspaceRoot: "/ws"})
	fmt.Printf("  linux（bwrap 挂、landlock 可用）→ runner=%s  enforcement=%s\n", c2.Argv[0], c2.Enforcement)

	// 两个都挂 → fail-closed，命令绝不裸跑。
	lin3 := NewProvider("linux")
	lin3.probeBwrap = func() bool { return false }
	lin3.probeLandlock = func() (SandboxEnforcement, bool) { return EnforcementFull, false }
	_, err3 := lin3.confine([]string{"bash", "-c", "echo hi"}, SandboxExecutionPolicy{Mode: ReadOnly, WorkspaceRoot: "/ws"})
	fmt.Printf("  linux（都不可用）→ %v\n", err3)

	// darwin：唯一候选，不探针直接选。
	dar := NewProvider("darwin")
	c4, _ := dar.confine([]string{"bash", "-c", "echo hi"}, SandboxExecutionPolicy{Mode: ReadOnly, WorkspaceRoot: "/ws"})
	fmt.Printf("  darwin（唯一候选 seatbelt）→ runner=%s  enforcement=%s\n", c4.Argv[0], c4.Enforcement)

	// 未知平台：无链 → fail-closed。
	unk := NewProvider("freebsd")
	_, err5 := unk.confine([]string{"bash", "-c", "echo hi"}, SandboxExecutionPolicy{Mode: ReadOnly, WorkspaceRoot: "/ws"})
	fmt.Printf("  freebsd（无链）→ %v\n", err5)
}

func scenario2ProfileWrap() {
	fmt.Println("\n=== 场景 2：三种 profile 包装 argv ===\n")

	// bwrap：workspace-write 时多 tmpfs + bind。
	b := append([]string{"bwrap"}, bwrapProfileArgs(SandboxExecutionPolicy{Mode: WorkspaceWrite, WorkspaceRoot: "/ws"})...)
	fmt.Printf("  bwrap workspace-write:\n    %s\n", strings.Join(b, " "))

	// landlock：grant 拼写。
	l := append([]string{"landlock-run"}, landlockProfileArgs(SandboxExecutionPolicy{Mode: WorkspaceWrite, WorkspaceRoot: "/ws"})...)
	fmt.Printf("  landlock workspace-write:\n    %s\n", strings.Join(l, " "))

	// seatbelt：SBPL profile，可写根用共享 writableRoots。
	s := append([]string{"sandbox-exec"}, seatbeltProfileArgs(SandboxExecutionPolicy{Mode: WorkspaceWrite, WorkspaceRoot: "/ws"})...)
	fmt.Printf("  seatbelt workspace-write:\n    %s\n", strings.Join(s, " "))

	// read-only 时三种 profile 都不含 workspace 可写。
	bRO := bwrapProfileArgs(SandboxExecutionPolicy{Mode: ReadOnly, WorkspaceRoot: "/ws"})
	fmt.Printf("  bwrap read-only（无 tmpfs/bind）:\n    %s\n", strings.Join(append([]string{"bwrap"}, bRO...), " "))
}

func scenario3Containment() {
	fmt.Println("\n=== 场景 3：writableRoots + 进程内栅栏 containment ===\n")

	// 用临时目录当 workspace，避免 Windows 路径分隔符问题。
	ws := filepath.Join(os.TempDir(), "dsh-ws")
	fmt.Printf("  workspaceRoot=%q\n", ws)

	ro := SandboxExecutionPolicy{Mode: ReadOnly, WorkspaceRoot: ws}
	ww := SandboxExecutionPolicy{Mode: WorkspaceWrite, WorkspaceRoot: ws}

	fmt.Printf("  writableRoots(read-only) = %v（空集）\n", writableRoots(ro))
	fmt.Printf("  writableRoots(workspace-write) = %v（workspace + /tmp + os 临时区去重）\n", writableRoots(ww))

	// 进程内栅栏：read-only 拒绝一切写。
	if _, err := checkedTarget(filepath.Join(ws, "a.txt"), ro); err != nil {
		fmt.Printf("  checkedTarget(read-only, %q) → %v\n", filepath.Join(ws, "a.txt"), err)
	}

	// workspace-write：workspace 内放行。
	if fresh, err := checkedTarget(filepath.Join(ws, "a.txt"), ww); err == nil {
		fmt.Printf("  checkedTarget(workspace-write, %q) → 放行，fresh=%q\n", filepath.Join(ws, "a.txt"), fresh)
	}

	// workspace-write：临时区内的文件即使不在 workspace 下，也会被放行
	// （因为 os.TempDir() 本身就在可写根集合里——workspace-write 承诺临时区可写）。
	inTemp := filepath.Join(os.TempDir(), "outside", "a.txt")
	if fresh, err := checkedTarget(inTemp, ww); err == nil {
		fmt.Printf("  checkedTarget(workspace-write, %q) → 放行（临时区本就可写）fresh=%q\n", inTemp, fresh)
	}

	// workspace-write：真正在可写根之外（用户主目录）→ 拒绝。
	home, _ := os.UserHomeDir()
	outside := filepath.Join(home, "outside-ws", "a.txt")
	if _, err := checkedTarget(outside, ww); err != nil {
		fmt.Printf("  checkedTarget(workspace-write, %q) → %v\n", outside, err)
	}

	// danger-full-access：无栅栏直通。
	if fresh, err := checkedTarget(outside, SandboxExecutionPolicy{Mode: DangerFullAccess, WorkspaceRoot: ws}); err == nil {
		fmt.Printf("  checkedTarget(danger-full-access, %q) → 直通 %q\n", outside, fresh)
	}

	// isPathUnder：词法快路径 + 身份回退（8.3 别名/大小写）。
	fmt.Printf("  isPathUnder(%q 在 %q 下, 大小写敏感) = %v\n", filepath.Join(ws, "x"), ws, isPathUnder(filepath.Join(ws, "x"), ws, true))
	fmt.Printf("  isPathUnder(%q 在 %q 下, 大小写不敏感) = %v\n", strings.ToUpper(filepath.Join(ws, "x")), ws, isPathUnder(strings.ToUpper(filepath.Join(ws, "x")), ws, false))
}

func scenario4Escalation() {
	fmt.Println("\n=== 场景 4：升级阶梯 + approveEscalation fail-closed ===\n")

	// 严格更宽：read-only → workspace-write 合法。
	if m, err := approveEscalation(WorkspaceWrite, ReadOnly, "需要写日志文件", "command", func() EscalationOutcome { return OutcomeAllowedOnce }, true); err == nil {
		fmt.Printf("  read-only → workspace-write（allowed-once）→ 授予 %q\n", m)
	}

	// 非更宽请求：绝不打扰人，直接报错。
	if _, err := approveEscalation(ReadOnly, WorkspaceWrite, "降级", "command", func() EscalationOutcome { return OutcomeAllowedOnce }, true); err != nil {
		fmt.Printf("  workspace-write → read-only（非更宽）→ %v\n", err)
	}

	// 无审批服务：fail-closed。
	if _, err := approveEscalation(WorkspaceWrite, ReadOnly, "需要写", "command", nil, true); err != nil {
		fmt.Printf("  read-only → workspace-write（无 approver）→ %v\n", err)
	}

	// agentless：fail-closed。
	if _, err := approveEscalation(WorkspaceWrite, ReadOnly, "需要写", "command", func() EscalationOutcome { return OutcomeAllowedOnce }, false); err != nil {
		fmt.Printf("  read-only → workspace-write（无 agent）→ %v\n", err)
	}

	// 用户拒绝。
	if _, err := approveEscalation(WorkspaceWrite, ReadOnly, "需要写", "command", func() EscalationOutcome { return OutcomeRejected }, true); err != nil {
		fmt.Printf("  read-only → workspace-write（用户拒绝）→ %v\n", err)
	}
}

func scenario5PolicyResolution() {
	fmt.Println("\n=== 场景 5：策略解析优先级（部署默认 → 会话覆盖 → 显式覆盖）===\n")

	svc := &SandboxPolicyService{defaultMode: ReadOnly, workspaceRoot: "/fallback"}

	// 无会话：部署默认。
	p1 := svc.resolve(nil, nil)
	fmt.Printf("  无会话 → mode=%s root=%s\n", p1.Mode, p1.WorkspaceRoot)

	// 有会话、无覆盖：会话 cwd 当 workspace 边界。
	sess := &Session{Cwd: "/ws"}
	p2 := svc.resolve(sess, nil)
	fmt.Printf("  会话 cwd=/ws、无覆盖 → mode=%s root=%s\n", p2.Mode, p2.WorkspaceRoot)

	// 会话日志有 sandbox/mode 事件：fold 出覆盖。
	sess.Events = []Event{
		{Type: "sandbox/mode", Data: map[string]interface{}{"mode": WorkspaceWrite}},
		{Type: "sandbox/mode", Data: map[string]interface{}{"mode": DangerFullAccess}},
	}
	ov := effectiveSandboxMode(sess.Events)
	fmt.Printf("  effectiveSandboxMode(两个 sandbox/mode) = %s（最后一个赢）\n", *ov)
	p3 := svc.resolve(sess, nil)
	fmt.Printf("  会话覆盖 → mode=%s root=%s\n", p3.Mode, p3.WorkspaceRoot)

	// 显式覆盖：最高优先级（审批授予的更宽模式）。
	explicit := WorkspaceWrite
	p4 := svc.resolve(sess, &explicit)
	fmt.Printf("  显式覆盖 workspace-write → mode=%s（即使会话覆盖是 danger-full-access 也压不过显式）\n", p4.Mode)
}

func main() {
	fmt.Println("=== sandbox：进程隔离 + 文件策略 + 升级 ===\n")
	scenario1PlatformChain()
	scenario2ProfileWrap()
	scenario3Containment()
	scenario4Escalation()
	scenario5PolicyResolution()

	fmt.Println("\n=== 关键点 ===")
	fmt.Println("1. 两层：内核级进程约束（confine 包装 argv，隔离不可信代码）vs 进程内 fs 栅栏（canonicalize-then-contain，约束模型控制的路径）")
	fmt.Println("2. 平台链：先按平台选链，多候选才探针仲裁；无可用 runner → SANDBOX_UNAVAILABLE fail-closed，命令绝不裸跑")
	fmt.Println("3. 三个模式：read-only（写空集）/ workspace-write（workspace + 临时区）/ danger-full-access（无栅栏）")
	fmt.Println("4. 升级阶梯：严格更宽 + 审批通道，非更宽绝不打扰人，无审批/无 agent 一律 fail-closed")
	fmt.Println("5. 策略解析：显式覆盖 > 会话 sandbox/mode 事件（日志 fold）> 部署默认")
	fmt.Println("6. denial 方言 vs runner 失败：runner failure=命令没跑，denial=约束生效挡住了")
}
