# Sandbox（进程隔离 + 文件策略 + 升级）

> 章节目标：从后端工程师视角理解 deepseek-harness 的沙箱——它如何隔离 agent 生成的不可信命令、如何约束文件写入、以及如何在"需要更大权限"时安全升级。配套可运行 Go 复现见 `main.go`。

## 一、核心认知：沙箱不是一层，是两层

这是本章最重要的点。后端同学听到"沙箱"容易只想到容器/cgroup 那一层，但 deepseek-harness 里有两层，防御的是**两种完全不同的威胁**：

| 层 | 是什么 | 防御的威胁 | 后端类比 |
|---|---|---|---|
| **内核级进程约束** | `ctx.sandbox.confine()` 包装 argv 交给 bwrap/Landlock/Seatbelt/windows-acl | **不可信代码**（模型让 bash 跑任意命令） | 容器 / seccomp / cgroup |
| **进程内 fs 栅栏** | `ctx.fs` 的 `checkedTarget()` 在受信代码里检查**模型控制的路径** | 模型让 write/edit 工具写越界路径 | 应用层权限检查（if 判断） |

**为什么需要两层**：

- bash 工具执行的是**任意 shell 命令**，命令体本身就是不可信的（可能是模型编的恶意命令），必须交给内核级隔离——这是 `ctx.shell` 的活。
- 而 fs 工具的 `write`/`edit` 操作体是**框架自己的代码**（open、rename），只有**目标路径**是模型控制的、不可信的。对"路径"这个面，canonicalize 后做 containment 检查就够了，不需要内核隔离。

关键区分（`fs-sandbox/src/index.ts` 开头就写明了）：

> 进程内栅栏是"受信代码对模型控制路径的策略检查"，**不是安全边界**（不是 kernel boundary）；内核级隔离不可信代码才是 `ctx.shell` 的活。

## 二、三个模式（SandboxMode）

文件效应策略的封闭词汇：

| 模式 | 能写什么 | 备注 |
|---|---|---|
| `read-only` | 什么都不写（只允许 `/dev/null` 之类必需 sink） | 默认值（fail-safe：想写必须显式 opt-in） |
| `workspace-write` | workspace 根 + `/tmp` + os 临时区 | 可写根来自 `writableRoots()` |
| `danger-full-access` | 全放行（无栅栏直通） | 绕过约束 |

其中 `read-only` 和 `workspace-write` 叫 **ConfinedSandboxMode**（被约束的模式）；`danger-full-access` 不算"被约束"。

## 三、平台 runner 链（先平台，后探针）

`PLATFORM_CHAINS`（`sandbox-local/src/index.ts:159`）：

```
linux  → [bwrap, landlock]   # 两个候选，探针仲裁
darwin → [seatbelt]          # 唯一候选，不探针直接选
win32  → [windows-acl]       # 唯一候选，不探针直接选
```

选择算法（`chainVerdict`）：

1. **先按平台选链**，再看链里有几个候选。
2. **唯一候选 → 不探针直接选**（执行时的拒绝仍 fail-closed，通过 stderr 签名识别）。
3. **多候选 → 按顺序功能探针仲裁**，第一个可用者胜出。探针是"真跑一次"（bwrap 真挂载、Landlock 真构建 ruleset），不是版本号判断——因为版本号会漏掉"有 syscall 但拒绝执行"的内核。
4. **全不可用 / 无链 → 抛 `SANDBOX_UNAVAILABLE`**，命令**绝不裸跑**。

**fail-closed 是贯穿全章的红线**：沙箱可用就包装，不可用就报错，绝不存在"悄悄不隔离直接跑"的中间态。

## 四、confine 做了什么（包装 argv）

`confine(argv, policy)` 不直接执行命令，而是返回**替代的 argv**，调用方用返回的 argv 去 spawn 原命令。这就是"包装"：

```
原 argv:   ["bash", "-c", "echo hi"]
包装后:    ["bwrap", "--ro-bind", "/", "/", ..., "--", "bash", "-c", "echo hi"]
                                            ^^ 分隔符：后面是原命令
```

三种 profile 构造（`profiles.ts`）：

- **bwrap**：`--ro-bind / /`（整根只读挂载）+ `--dev /dev` + `--proc /proc` + `--die-with-parent`；workspace-write 再加 `--tmpfs /tmp`（临时区隔离）+ `--bind workspace workspace`（workspace 可写）。
- **Landlock**：allow-list grant——`readOnly=/`，`readWrite` 含 `/dev/null`，workspace-write 再加 `/tmp` + workspace。由原生 launcher（`@deepseek-ai/node-addon-landlock-run`）执行。
- **Seatbelt**：SBPL profile——`(allow default)` + `(deny file-write*)` 默认全禁写，再 `(allow file-write* ...)` 放开 `/dev/null` 和可写根。可写根来自**同一个** `writableRoots()`，保证 Seatbelt 和进程内 fs 栅栏永不漂移。

`ConfinedArgv` 除了包装 argv，还携带三样"证据"：

1. **enforcement**（full / partial）——本次执行的完整度。windows-acl 因 Everyone 必须留在限制列表 + NTFS 硬链接别名，只能承诺 partial。
2. **denialSignatures**（拒绝方言）——被拒绝的文件效应在 stderr 上产生的子串（bwrap 是 `read-only file system`、Landlock 是 `permission denied`、Seatbelt 是 `operation not permitted`）。消费者据此推断"是拒绝"。
3. **runnerFailureRules**（runner 失败规则）——runner 自身的致命诊断。

**关键区分**（后端视角很重要）：

> **runner failure = 命令根本没跑**（runner 启动失败，如 profile 被拒）；
> **denial = 约束生效，挡住了命令**（命令跑了但被内核拒绝）。

两者 stderr 都要看，但含义完全相反。这需要一个明确的分类顺序：先排除 runner failure，再匹配 denial 签名。

## 五、writableRoots：可写根的唯一归属地

`workspace-write` 的语义就是"workspace 根 + 平台临时区"，这个含义只有 `roots.ts` 里的 `writableRoots()` 一处定义：

```
workspace-write 可写 = { workspaceRoot, /tmp, os.tmpdir() } 去重 + canonical
read-only 可写 = ∅（空集）
```

**为什么放这里**：Seatbelt 的 grant 和进程内 fs 栅栏都用它推导可写根，同一处定义保证两者永不漂移（不会出现"write 工具写不了 /tmp 但 bash 能写"的不对称）。

`canonicalPath` 用 native realpath 解析 symlink（`/tmp` 在 darwin 是 `/private/tmp`）；解析失败返回原样（缺失的根匹配不到任何东西，直到它存在——保守结果）。

## 六、进程内 fs 栅栏：canonicalize-then-contain

`fs-sandbox/src/index.ts` 的 `checkedTarget()`：

```
mode == danger-full-access → 直通（无栅栏）
mode == read-only          → 拒绝（FS_SANDBOX_DENIED）
mode == workspace-write    → 重新 canonicalize → isPathUnder 判断是否在可写根内
                             在 → 返回 FRESH target（不是旧 target）
                             不在 → 拒绝
```

三个要点：

1. **现在重新 canonicalize**（不是用工具之前解析好的 target）——抓"从工具解析到写之间被换掉的 symlink 祖先"。
2. **返回 fresh target 去写**——被检查的身份就是被写的那一个，避免"检查这里、写那里"的 TOCTOU。
3. **拒绝抛结构化错误** `FS_SANDBOX_DENIED`——因为进程内栅栏**精确知道自己拒绝了什么**，不需要像 bash 那样从内核 stderr 推断文本。

`isPathUnder`（`containment.ts`）两段式：

- **词法快路径**：target 是 root 或它的词法后代（大小写按平台）。
- **文件系统身份回退**：词法拼写不同时，向上遍历已存在祖先，用 `dev+ino`（`os.SameFile`）比对身份——识别 Windows 8.3 短名/长名别名和大小写，而不把 containment 弱化成纯文本近似。

## 七、升级（Escalation）：严格更宽 + 审批

模型在 `read-only` 下想写文件，不能直接放行，也不能让模型裸降级。升级是一套**先校验、再审批、后映射**的 fail-closed 序列：

`WIDER_MODES` 严格更宽阶梯：

```
read-only       → [workspace-write, danger-full-access]
workspace-write → [danger-full-access]
```

`approveEscalation()` 的顺序（`escalation.ts:157`）：

1. **严格更宽校验**——requested 必须在 effective 的 WIDER_MODES 里，否则**直接报错、绝不打扰人**。
2. **无审批服务 / 无 agent → fail-closed 报错**。
3. 走审批通道（复用 HITL 的 `ctx.approval.request`，见 human-in-the-loop 章）。
4. 映射结果：`allowed-once` → 授予更宽模式（只 stamp 到**这一次**调用）；`rejected`/`cancelled`/`unavailable` → 各自报错。

升级授予的模式是**单次调用**的（per-call），不是永久改会话——下一次调用回到原模式。

## 八、策略解析：三层优先级

`ctx.sandboxPolicy.resolve()` 解析一次调用的模式和工作区根，优先级（`sandbox-policy/src/index.ts:135`）：

```
显式覆盖（审批授予的更宽模式）
  > 会话最后一个 sandbox/mode 事件（日志 fold）
    > 部署默认（Config，默认 read-only）
```

会话的 `sandbox/mode` 覆盖是**日志事件**（`session-mode.ts`）：重放日志就是状态，重启后覆盖仍在，两个会话互不可见，没有外部配置存储。`effectiveSandboxMode` 是纯 fold（从后往前找最后一个 `sandbox/mode`）。

工作区根：会话 cwd 是 workspace-write 边界；配置根是 agentless 调用的回退。

## 九、后端视角速记

| 概念 | 后端等价物 |
|---|---|
| 内核级进程约束 | 容器 / seccomp / cgroup / Landlock |
| 进程内 fs 栅栏 | 应用层权限检查（if 判断） |
| confine 包装 argv | 装饰器模式（spawn 前改 argv） |
| fail-closed | 宁可拒绝也不裸跑 |
| 平台链 + 探针 | 多实现按平台路由 + 能力探测 |
| denial 方言 | 按后端各自的错误语义分类 stderr |
| 升级阶梯 | 权限提权需审批，strictly-wider |
| sandbox/mode 日志 fold | 事件溯源 + 投影状态 |

## 十、deepseek-harness 源码对应

| 概念 | 源码位置 |
|---|---|
| SandboxProvider 抽象 + 三模式 + ConfinedArgv | `packages/sandbox/sandbox/src/index.ts` |
| 平台链 + confine + fail-closed + 探针 | `packages/sandbox/sandbox-local/src/index.ts` |
| 三种 profile 构造 | `packages/sandbox/sandbox-local/src/profiles.ts` |
| writableRoots + canonicalPath | `packages/sandbox/sandbox/src/roots.ts` |
| WIDER_MODES + approveEscalation | `packages/sandbox/sandbox/src/escalation.ts` |
| 策略解析（三层优先级） | `packages/sandbox/sandbox-policy/src/index.ts` |
| effectiveSandboxMode（日志 fold） | `packages/sandbox/sandbox-policy/src/session-mode.ts` |
| isPathUnder（词法 + 身份回退） | `packages/fs/fs-sandbox/src/containment.ts` |
| checkedTarget（canonicalize-then-contain） | `packages/fs/fs-sandbox/src/index.ts` |
| Landlock 原生 launcher（机制非策略） | `native/landlock-run/docs/architecture.md` |

## 十一、复现覆盖 + 未复现

**已覆盖**：
- 平台链选择 + 探针仲裁 + fail-closed（`SANDBOX_UNAVAILABLE`）
- 三种 profile 包装 argv（bwrap/Landlock/Seatbelt）
- writableRoots（read-only 空集 / workspace-write 三根去重）
- 进程内栅栏 checkedTarget 的四个分支（read-only 拒绝 / workspace-write 放行+越界拒绝 / danger-full-access 直通）
- isPathUnder（词法快路径 + 身份回退）
- WIDER_MODES 阶梯 + approveEscalation 的 fail-closed 序列
- 策略解析三层优先级 + effectiveSandboxMode 日志 fold

**未复现（真实实现有、demo 里省略）**：
- 真正的内核执行（bwrap/Landlock/Seatbelt 实际隔离进程）——demo 只构造 argv，不真 spawn
- Landlock 原生 launcher 的 exit 125 + 部分执行（partial enforcement）自报告
- windows-acl 的完整受限 token / SID / DACL 管理（每个 workspace 一个驻留 grant + 每个会话一个随机私有临时目录 grant）
- 双通道审批的完整链路（升级审批实际走 HITL，见 human-in-the-loop 章）
- `approval/*` 审计事件的 durable 持久化
- 模型可见的 denial marker / escalation hint marker（`[sandbox: file access denied ...]`）
- 系统提示词里注入的 policy context（`renderPolicyContext`，模型看到"当前只读"这段描述）

## 十二、面试要点速记

1. 沙箱是两层：内核级进程约束（隔离不可信代码）vs 进程内 fs 栅栏（约束模型控制的路径），防御不同威胁。
2. fail-closed 红线：沙箱不可用就抛 `SANDBOX_UNAVAILABLE`，命令绝不裸跑。
3. 平台链：先平台后探针，唯一候选不探针，多候选功能探针仲裁。
4. runner failure（命令没跑）≠ denial（约束生效挡住），分类顺序：先排 runner failure，再匹配 denial 签名。
5. 升级是严格更宽 + 审批 + 单次调用，非更宽请求绝不打扰人，无审批/无 agent fail-closed。
6. 策略优先级：显式覆盖 > 会话 sandbox/mode 日志 fold > 部署默认。
