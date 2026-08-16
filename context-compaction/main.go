package main

// deepseek-harness Context Compaction 完整复现（非简化版）
// 章节：context-compaction
//
// 对照真实源码逐段还原：
//   estimate.ts     → 启发式估算（4 字符/token + block/role 开销）
//   token-meter     → 锚点机制（provider usage 采信 + 保守校验 + 增量）
//   config.ts       → 预算（80% 阈值 / 16% 保留 / 默认值 / 校验）
//   region.ts       → selectCompactableRange（tool-call 配平切割）+ compactSurfaceRegion（事务）
//   summarizer.ts   → 摘要指令 + 复用前缀 + frameSummary + 摘要更小校验
//   index.ts        → compactIfNeeded（pressure/overflow + prune + 重试循环）
//
// 运行：go run main.go

import "fmt"

// ============ 1. 基础类型（对应 dsh-llm）============

type ContentBlock struct {
	Type      string         // text / tool-call / tool-result
	Text      string         // text 块内容
	Name      string         // tool-call 名
	Arguments string         // tool-call 参数
	Content   []ContentBlock // tool-result 嵌套内容
}

// TokenUsage provider 响应里回报的真实用量（4 个桶）
type TokenUsage struct {
	InputTokens      int
	CacheReadTokens  int
	CacheWriteTokens int
	OutputTokens     int
}

type Message struct {
	Role     string
	Content  []ContentBlock
	ToolCalls int         // assistant 消息带几个 tool-call（用于配平）
	Usage    *TokenUsage // assistant/message 可能带 usage
}

// Header 请求信封（system prompt + tool schema），用于估算头部 token
type Header struct {
	System string
	Tools  string // 简化：把 tool schema 序列化成字符串参与估算
}

type Event struct {
	Seq           int
	Type          string // turn/start, request/header, user/message, assistant/message, tool/result, compaction/start, compaction/summary, compaction/end
	Message       Message
	Header        *Header
	Summary       string
	ShadowedRange *Range
	CompactionID  string
}

type Range struct{ Start, End int }

// ============ 2. 启发式估算（estimate.ts）============

const (
	charsPerToken = 4 // 每 4 字符 ≈ 1 token
	blockOverhead = 4 // 每个内容块的 JSON 结构开销
	roleOverhead  = 4 // 每条消息的 role 字段开销
)

// estimateContent 对应 estimate.ts:26-49，对三种 block 分别定价
func estimateContent(blocks []ContentBlock) int {
	tokens := 0
	for _, b := range blocks {
		switch b.Type {
		case "text", "reasoning":
			tokens += ceilDiv(len([]rune(b.Text)), charsPerToken) + blockOverhead
		case "tool-call":
			tokens += ceilDiv(len([]rune(b.Name)), charsPerToken) +
				ceilDiv(len([]rune(b.Arguments)), charsPerToken) + blockOverhead
		case "tool-result":
			tokens += estimateContent(b.Content) + blockOverhead
		default:
			tokens += blockOverhead
		}
	}
	return tokens
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// estimateMessage 对应 estimate.ts:56-58
func estimateMessage(m Message) int {
	return estimateContent(m.Content) + roleOverhead
}

// estimateHeader 对应 estimate.ts:85-87（system + tools）
func estimateHeader(h *Header) int {
	if h == nil {
		return 0
	}
	system := 0
	if h.System != "" {
		system = ceilDiv(len([]rune(h.System)), charsPerToken) + roleOverhead
	}
	tools := 0
	if h.Tools != "" {
		tools = ceilDiv(len([]rune(h.Tools)), charsPerToken) + blockOverhead
	}
	return system + tools
}

// ============ 3. usageTokens（token-meter/index.ts:44-49）============

// 汇总 provider usage 的四个桶，不重复计算 reasoning 输出
func usageTokens(u TokenUsage) int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens
}

// ============ 4. Session（append-only 日志 + surface 投影层）============

type Session struct {
	events  []Event
	surface []int // surface 节点的 seq（只有 user/assistant/tool-result）
	header  *Header
}

func (s *Session) append(e Event) Event {
	e.Seq = len(s.events)
	s.events = append(s.events, e)
	return e
}

// appendSurface 追加 surface 消息 + 记录 tool-call 配平
func (s *Session) appendSurface(e Event) {
	appended := s.append(e) // 接收返回值，拿到正确 Seq
	s.surface = append(s.surface, appended.Seq)
}

func (s *Session) appendHeader(h *Header) {
	s.header = h
	s.append(Event{Type: "request/header", Header: h})
}

// ============ 5. TokenMeter：锚点机制（token-meter/index.ts:116-310）============

type SurfaceNode struct {
	Seq    int
	Tokens int
}

type Measurement struct {
	Nodes []SurfaceNode
	Total int
}

type Anchor struct {
	tokens        int // 锚点总 token
	surfaceTokens int // 锚点时刻的 surface token
	kind          string
}

type TokenMeter struct {
	anchor *Anchor
}

// surfaceSnapshot 从 s.surface（权威投影层）重建节点定价和总 token。
// 关键：surface 是权威，压缩会正确更新它；这里每次都从 surface 重建，
// 而不是独立累积，避免压缩后 meter 与 surface 脱节。
func (m *TokenMeter) surfaceSnapshot(s *Session) ([]SurfaceNode, int) {
	nodes := make([]SurfaceNode, 0, len(s.surface))
	total := 0
	for _, seq := range s.surface {
		e := s.events[seq]
		t := estimateMessage(e.Message)
		nodes = append(nodes, SurfaceNode{Seq: seq, Tokens: t})
		total += t
	}
	return nodes, total
}

// measure 对应 index.ts:116-147：锚点 + 增量
func (m *TokenMeter) measure(s *Session) Measurement {
	nodes, surfaceTokens := m.surfaceSnapshot(s)
	headerTokens := estimateHeader(s.header)

	if m.anchor != nil {
		// 命中锚点：锚点 + 之后 surface 增量
		delta := surfaceTokens - m.anchor.surfaceTokens
		return Measurement{Nodes: nodes, Total: m.anchor.tokens + delta}
	}
	// 无锚点：全量启发式
	return Measurement{Nodes: nodes, Total: headerTokens + surfaceTokens}
}

// onAssistantUsage 对应 index.ts:232-249：收到 assistant/message 带 usage 时更新锚点
func (m *TokenMeter) onAssistantUsage(s *Session, e Event) {
	if e.Message.Usage == nil {
		return
	}
	_, surfaceTokens := m.surfaceSnapshot(s)
	headerTokens := estimateHeader(s.header)
	providerTokens := usageTokens(*e.Message.Usage)
	estimatedAnchor := headerTokens + surfaceTokens
	// 保守校验：provider 真值 >= 启发式估算才采信，否则用估算（防止缓存命中低估）
	if providerTokens >= estimatedAnchor {
		m.anchor = &Anchor{tokens: providerTokens, surfaceTokens: surfaceTokens, kind: "usage"}
	} else {
		m.anchor = &Anchor{tokens: estimatedAnchor, surfaceTokens: surfaceTokens, kind: "estimated"}
	}
}

// ============ 6. 预算（config.ts）============

const (
	defaultThresholdRatio    = 0.8
	defaultRetainRatio       = 0.16
	defaultMaxTokens         = 8192
	defaultCompactionRetries = 1
	defaultMaxOverflowRetries = 1
)

type Spec struct {
	ThresholdTokens     int
	RetainTokens        int
	MaxTokens           int
	CompactionRetries   int
	MaxOverflowRetries  int
}

func resolveSpec(window int) Spec {
	threshold := int(float64(window) * defaultThresholdRatio)
	retain := int(float64(window) * defaultRetainRatio)
	// 校验 retain < threshold（config.ts:148-154）
	if retain >= threshold {
		panic(fmt.Sprintf("retainTokens %d 必须 < thresholdTokens %d", retain, threshold))
	}
	return Spec{threshold, retain, defaultMaxTokens, defaultCompactionRetries, defaultMaxOverflowRetries}
}

// ============ 7. 选范围 + tool-call 配平（region.ts:98-134）============

// balanceAt 计算 surface 每个位置处的"未闭合 tool-call 数"
// assistant 带 N 个 tool-call → +N；tool/result → -1
func balanceAt(s *Session) []int {
	bal := make([]int, len(s.surface))
	open := 0
	for i, seq := range s.surface {
		e := s.events[seq]
		bal[i] = open
		switch e.Type {
		case "assistant/message":
			open += e.Message.ToolCalls
		case "tool/result":
			open--
		}
	}
	return bal
}

// toolPairingBalancedBefore 切割点 idx 处是否配平（不切断 tool-call/result 对）
func toolPairingBalancedBefore(bal []int, idx int) bool {
	return bal[idx] == 0
}

// selectCompactableRange 对应 region.ts:98-134：
// 尾部累计找 keepFromIdx，再向前找配平切割点
func selectCompactableRange(s *Session, m Measurement, retainTokens int) *Range {
	if len(m.Nodes) == 0 {
		return nil
	}
	bal := balanceAt(s)

	// 从尾部往前累计，保留 retainTokens 的尾部
	accumulated := 0
	keepFromIdx := len(m.Nodes)
	for i := len(m.Nodes) - 1; i >= 0; i-- {
		accumulated += m.Nodes[i].Tokens
		keepFromIdx = i
		if accumulated >= retainTokens {
			break
		}
	}
	if keepFromIdx == 0 {
		return nil
	}
	// 向前找配平切割点（不切断 tool-call 对）
	for keepFromIdx > 0 {
		if toolPairingBalancedBefore(bal, keepFromIdx) {
			break
		}
		keepFromIdx--
	}
	if keepFromIdx == 0 {
		return nil
	}
	return &Range{Start: s.surface[0], End: s.surface[keepFromIdx-1]}
}

// ============ 8. 摘要（summarizer.ts）============

const checkpointPreamble = "This is an automatically generated checkpoint condensing an earlier span of the conversation to free up context."

// summarize 模拟 LLM 摘要：复用前缀（system+tools+shadowed 消息）+ 追加摘要指令
// 真实实现是 ctx.llm.stream 流式调用 + COMPACTION_INSTRUCTION，这里模拟返回结构化摘要
func summarize(shadowed []Event, header *Header) string {
	// 真实：复用 header.System + header.Tools + shadowed 消息 + 追加 COMPACTION_INSTRUCTION
	// 让辅助调用成为上次请求的真前缀，复用 provider KV cache
	summary := fmt.Sprintf("## Primary Request and Intent\n- 用户要排查性能隐患\n## Key Technical Concepts\n- Go 并发、goroutine 泄漏\n## Files and Code\n- 扫描了 %d 条历史消息\n", len(shadowed))
	return summary
}

// frameSummary 对应 summarizer.ts:189-195：包上 preamble + 标签
func frameSummary(summary string) string {
	return checkpointPreamble + "\n\n<compacted-summary>\n" + summary + "\n</compacted-summary>"
}

// ============ 9. 压缩事务（region.ts:152-254）============

type CompactionResult struct {
	CompactionID    string
	Summary         string
	ShadowedRange   *Range
	ShadowedSeqs    []int
	ShadowedTokens  int
	StartSeq        int
	SummarySeq      int
	EndSeq          int
}

// compactSurfaceRegion 对应 region.ts:152-254：加锁→摘要→替换→解锁
func compactSurfaceRegion(s *Session, meter *TokenMeter, start, end int) *CompactionResult {
	// 校验 region 在 surface 上
	startIdx, endIdx := -1, -1
	for i, seq := range s.surface {
		if seq == start { startIdx = i }
		if seq == end { endIdx = i }
	}
	if startIdx == -1 || endIdx == -1 || startIdx > endIdx {
		panic("compactRegion: 非法 range")
	}

	shadowedSeqs := append([]int{}, s.surface[startIdx:endIdx+1]...)

	// ① 加锁：append compaction/start（同步相邻，是持久锁）
	compactionID := fmt.Sprintf("c%d", len(s.events))
	startEvent := s.append(Event{Type: "compaction/start", CompactionID: compactionID})

	// ② 准备：选节点 + 构建摘要输入
	var shadowedEvents []Event
	var shadowedTokens int
	for _, seq := range shadowedSeqs {
		e := s.events[seq]
		shadowedEvents = append(shadowedEvents, e)
		shadowedTokens += estimateMessage(e.Message)
	}

	// ③ 摘要 + frameSummary + 校验摘要必须更小（region.ts:374-378）
	rawSummary := summarize(shadowedEvents, s.header)
	framed := frameSummary(rawSummary)
	framedTokens := ceilDiv(len([]rune(framed)), charsPerToken) + roleOverhead
	if framedTokens >= shadowedTokens {
		panic(fmt.Sprintf("summary 没有比被压内容小：%d >= %d", framedTokens, shadowedTokens))
	}

	// ④ 记录摘要（log-only）
	summaryEvent := s.append(Event{Type: "compaction/summary", CompactionID: compactionID, Summary: framed, ShadowedRange: &Range{start, end}})

	// ⑤ 替换：append user/message 带 surfaceOp replace（唯一 surface 变更）
	checkpoint := Message{Role: "user", Content: []ContentBlock{{Type: "text", Text: framed}}}
	checkpointEvent := s.append(Event{Type: "user/message", Message: checkpoint})
	// 替换 surface：移除 [startIdx, endIdx]，在 startIdx 位置插入 checkpoint
	newSurface := make([]int, 0, len(s.surface)-len(shadowedSeqs)+1)
	newSurface = append(newSurface, s.surface[:startIdx]...)
	newSurface = append(newSurface, checkpointEvent.Seq)
	newSurface = append(newSurface, s.surface[endIdx+1:]...)
	s.surface = newSurface

	// ⑥ 解锁：append compaction/end
	endEvent := s.append(Event{Type: "compaction/end", CompactionID: compactionID})

	return &CompactionResult{
		CompactionID:  compactionID,
		Summary:       framed,
		ShadowedRange: &Range{start, end},
		ShadowedSeqs:  shadowedSeqs,
		ShadowedTokens: shadowedTokens,
		StartSeq:      startEvent.Seq,
		SummarySeq:    summaryEvent.Seq,
		EndSeq:        endEvent.Seq,
	}
}

// ============ 10. compactIfNeeded（index.ts:258-332）============

type Trigger int

const (
	Pressure Trigger = iota // 预防
	Overflow                // 补救
)

type Engine struct {
	meter  *TokenMeter
	window int
}

// prune 无模型删工具结果（简化：这里 no-op，真实是 toolResultPruner）
func (e *Engine) prune(s *Session) {
	// 真实：删除可丢弃的工具结果，不调 LLM。演示里不实现细节。
}

func (e *Engine) compactIfNeeded(s *Session, trigger Trigger) *CompactionResult {
	measurement := e.meter.measure(s)
	spec := resolveSpec(e.window)

	if trigger == Overflow {
		// 溢出：跳过阈值，强制压一次（retain=0 全压）
		e.prune(s)
		measurement = e.meter.measure(s)
		r := selectCompactableRange(s, measurement, 0)
		if r == nil {
			return nil
		}
		return compactSurfaceRegion(s, e.meter, r.Start, r.End)
	}

	// 压力：低于阈值不压
	if measurement.Total < spec.ThresholdTokens {
		return nil
	}
	// 先无模型 prune，再重测
	e.prune(s)
	measurement = e.meter.measure(s)
	if measurement.Total < spec.ThresholdTokens {
		return nil
	}
	// 循环：压一次重测一次
	var result *CompactionResult
	for attempt := 0; attempt <= spec.CompactionRetries; attempt++ {
		r := selectCompactableRange(s, measurement, spec.RetainTokens)
		if r == nil {
			break
		}
		result = compactSurfaceRegion(s, e.meter, r.Start, r.End)
		measurement = e.meter.measure(s)
		if measurement.Total < spec.ThresholdTokens {
			return result
		}
	}
	return result
}

// ============ 11. main：完整演示 ============

func main() {
	s := &Session{}
	meter := &TokenMeter{}
	eng := &Engine{meter: meter, window: 160} // 窗口 100 token

	// 请求信封（system + tools），决定头部 token
	s.appendHeader(&Header{
		System: "你是 Go 后端开发助手，帮助排查性能问题",
		Tools:  "bash, write, ask_user_question",
	})

	fmt.Println("=== 模拟长对话：消息不断累积，触发压缩 ===")
	fmt.Println()

	// 多轮对话（含 tool-call 对），每轮 assistant 带 tool-call + tool-result
	type turn struct {
		assistantText  string
		toolCalls      int
		toolResultText string
		usage          *TokenUsage
	}
	turns := []turn{
		{"我先扫描项目目录结构，看看有哪些文件和包", 1, "扫描完成，发现 main.go、util.go、handler.go 三个核心文件", &TokenUsage{InputTokens: 30, OutputTokens: 15}},
		{"接着检查 goroutine 的使用情况，找泄漏风险", 1, "发现 3 处 go func 直接启动协程，缺少退出机制", &TokenUsage{InputTokens: 30, OutputTokens: 15}},
		{"继续排查内存分配，找大对象和频繁分配", 1, "发现 2 处大对象分配，1 处循环内频繁分配", &TokenUsage{InputTokens: 30, OutputTokens: 15}},
		{"再检查锁竞争，看有没有 mutex 滥用", 1, "发现 1 处全局锁竞争，可能影响并发性能", &TokenUsage{InputTokens: 30, OutputTokens: 15}},
		{"补充检查 channel 的使用，看有没有阻塞风险", 1, "发现 1 处 channel 未关闭导致阻塞", &TokenUsage{InputTokens: 30, OutputTokens: 15}},
		{"检查 defer 的使用，看有没有资源泄漏", 1, "发现 2 处文件句柄未及时关闭", &TokenUsage{InputTokens: 30, OutputTokens: 15}},
		{"检查错误处理，看有没有吞掉错误", 1, "发现 3 处错误被直接忽略", &TokenUsage{InputTokens: 30, OutputTokens: 15}},
		{"检查 context 传递，看有没有超时泄漏", 1, "发现 1 处 context 没有透传", &TokenUsage{InputTokens: 30, OutputTokens: 15}},
		{"检查 slice 扩容，看有没有性能浪费", 1, "发现 2 处 slice 未预分配容量", &TokenUsage{InputTokens: 30, OutputTokens: 15}},
		{"最后整理成报告写入文件", 1, "报告已生成，共发现 15 处问题", &TokenUsage{InputTokens: 30, OutputTokens: 15}},
	}

	for i, t := range turns {
		// assistant 带 tool-call
		assistantMsg := Message{
			Role:      "assistant",
			Content:   []ContentBlock{{Type: "text", Text: t.assistantText}},
			ToolCalls: t.toolCalls,
			Usage:     t.usage,
		}
		s.appendSurface(Event{Type: "assistant/message", Message: assistantMsg})
		// 模拟 provider usage 回来，更新锚点
		meter.onAssistantUsage(s, s.events[len(s.events)-1])

		// tool-result
		s.appendSurface(Event{Type: "tool/result", Message: Message{Role: "tool", Content: []ContentBlock{{Type: "text", Text: t.toolResultText}}}})

		// 测量
		before := meter.measure(s).Total
		fmt.Printf("第 %d 轮后：surface=%d 条，约 %d token（锚点=%v）\n", i+1, len(s.surface), before, meter.anchor != nil)

		// 触发压缩检查
		if before >= resolveSpec(eng.window).ThresholdTokens {
			r := eng.compactIfNeeded(s, Pressure)
			if r != nil {
				after := meter.measure(s).Total
				fmt.Printf("  ★ 触发压缩：shadowed %d 条 → 摘要，token %d → %d\n", len(r.ShadowedSeqs), before, after)
			}
		}
	}

	fmt.Println()
	fmt.Println("=== 压缩后，模型看到的历史（投影层）===")
	for _, seq := range s.surface {
		e := s.events[seq]
		switch e.Type {
		case "user/message":
			text := ""
			for _, b := range e.Message.Content {
				text += b.Text
			}
			// 判断是不是 checkpoint
			if len(text) > 60 {
				fmt.Printf("  [摘要节点] %s...\n", text[:60])
			} else {
				fmt.Printf("  %s\n", text)
			}
		case "assistant/message":
			fmt.Printf("  [assistant] %s\n", e.Message.Content[0].Text)
		case "tool/result":
			fmt.Printf("  [tool] %s\n", e.Message.Content[0].Text)
		}
	}

	fmt.Println()
	fmt.Println("=== 完整日志（append-only，原始事件 + compaction 标记都在）===")
	for _, e := range s.events {
		switch e.Type {
		case "compaction/start":
			fmt.Printf("  seq=%d compaction/start\n", e.Seq)
		case "compaction/summary":
			fmt.Printf("  seq=%d compaction/summary\n", e.Seq)
		case "compaction/end":
			fmt.Printf("  seq=%d compaction/end\n", e.Seq)
		default:
			// 省略普通消息的逐条打印，只统计
		}
	}
	// 统计
	summaryCount, normalCount := 0, 0
	for _, e := range s.events {
		switch e.Type {
		case "user/message", "assistant/message", "tool/result":
			normalCount++
		case "compaction/summary":
			summaryCount++
		}
	}
	fmt.Printf("  普通消息事件：%d 条（全部保留，未删除）\n", normalCount)
	fmt.Printf("  compaction 摘要：%d 条\n", summaryCount)
	fmt.Println()
	fmt.Println("说明：原始事件一条没删，都在 log 里；压缩只改 surface（投影层）。")
}
