package main

// deepseek-harness agent-loop 完整复现
// 章节：agent-loop
//
// 对照真实源码：
//   core/agent-loop/src/agent.ts:210-223  kick（外层 while(turn) 驱动）
//   core/agent-loop/src/agent.ts:246-330  turn（回合状态机）
//   core/agent-loop/src/agent.ts:225-243  preStep（claim + assemble + agent/pre-step waterfall）
//   core/agent-loop/src/agent.ts:332-399  step（buildRequest → llm/stream → tool calls）
//   core/session/src/index.ts:726-747    deriveMessages（从日志投影历史）
//
// 运行：go run main.go

import "fmt"

// ============ 基础类型 ============

type Event struct {
	Seq  int
	Type string // turn/start, step/start, user/message, assistant/chunk, assistant/message, tool/call, tool/result, step/end, turn/end
	Data string
}

type Session struct {
	events []Event
}

// append 落日志并打印，让事件序列可见
func (s *Session) append(t string, data string) Event {
	e := Event{Seq: len(s.events), Type: t, Data: data}
	s.events = append(s.events, e)
	fmt.Printf("    [log] seq=%d %-20s %s\n", e.Seq, t, data)
	return e
}

type ToolCall struct {
	Name string
	Args map[string]string
}

type Message struct {
	Role      string
	Text      string
	ToolCalls []ToolCall
}

// ============ LLM（一次调用返回一段：答案 或 tool-call）============

type LLM interface {
	Chat(history []Message) Message
}

type FakeLLM struct {
	replies []Message
	step    int
}

func (f *FakeLLM) Chat(history []Message) Message {
	if f.step < len(f.replies) {
		m := f.replies[f.step]
		f.step++
		return m
	}
	return Message{Role: "assistant", Text: "完成"}
}

// ============ Agent：turn/step 状态机 ============

type Phase struct {
	step int
}

type Agent struct {
	session      *Session
	inbox        []Message // 输入队列
	llm          LLM
	phase        Phase
	preStepHooks []PreStepHook // agent/pre-step waterfall
}

// PreStepHook 是 agent/pre-step waterfall 的一环：enter（放行）/ reject（拒绝）
type PreStepHook func(messages []Message) PreStepDecision

type PreStepDecision struct {
	kind     string // enter / reject
	messages []Message
}

// ============ deriveMessages（从日志投影历史）============

// deriveMessages 对应 session/src/index.ts:726-747：
// 从 append-only 日志投影出模型看到的历史（model-visible ⟺ logged）
func (a *Agent) deriveMessages() []Message {
	var msgs []Message
	for _, e := range a.session.events {
		switch e.Type {
		case "user/message", "assistant/message", "tool/result":
			msgs = append(msgs, Message{Role: e.Type, Text: e.Data})
		}
	}
	return msgs
}

// ============ preStep（对应 agent.ts:225-243）============

func (a *Agent) preStep() PreStepDecision {
	// claim 输入（简化：取 inbox 全部）
	claimed := append([]Message{}, a.inbox...)
	a.inbox = nil

	// agent/pre-step waterfall：遍历 hook，reject 短路，全 enter 则放行
	for _, hook := range a.preStepHooks {
		d := hook(claimed)
		if d.kind == "reject" {
			return d
		}
	}
	return PreStepDecision{kind: "enter", messages: claimed}
}

// step（对应 agent.ts:332-399）：一次模型请求 + 执行它调的工具。
// 注意：真实源码 step() 里的 while 是"重试循环"（request-error 时 retry），
// 不是"工具结果再问模型"的循环——工具结果要再问模型是 turn 外层循环的下一个 step。
func (a *Agent) step() *string {
	// buildRequest：deriveMessages 投影历史 + 组装请求
	history := a.deriveMessages()

	// 一次模型请求
	msg := a.llm.Chat(history)

	// 模拟流式：拆成 chunk 落日志（真实是 llm/stream 逐 chunk append assistant/chunk）
	for _, r := range msg.Text {
		a.session.append("assistant/chunk", string(r))
	}

	// assistant/message 落日志
	a.session.append("assistant/message", msg.Text)

	// 模型要调工具吗？
	if len(msg.ToolCalls) == 0 {
		return strPtr("completed") // 不调工具 → step 完成
	}

	// 执行工具（tools/call → 工具执行 → tools/result）
	for _, call := range msg.ToolCalls {
		a.session.append("tool/call", call.Name)
		result := executeTool(call.Name)
		a.session.append("tool/result", result)
	}

	// 工具结果要再问模型 → 返回 nil（不 completed），由 turn 外层循环开下一个 step
	return nil
}

func strPtr(s string) *string { return &s }

func executeTool(name string) string {
	return name + " 的输出"
}

// ============ turn（对应 agent.ts:246-330）============

func (a *Agent) turn() bool {
	a.session.append("turn/start", "turn")

	// 第一个 step 前：claim 输入 + agent/pre-step waterfall（reject/enter）
	decision := a.preStep()
	if decision.kind == "reject" {
		a.session.append("turn/end", "blocked（pre-step reject）")
		return false
	}
	if len(decision.messages) == 0 {
		a.session.append("turn/end", "completed（空输入）")
		return false
	}

	first := true
	for {
		step := a.phase.step + 1
		a.session.append("step/start", fmt.Sprintf("step %d", step))
		a.phase.step = step

		// 只有第一个 step 落 user/message（后续 step 靠 deriveMessages 投影工具结果）
		if first {
			for _, m := range decision.messages {
				a.session.append("user/message", m.Text)
			}
			first = false
		}

		// step：一次模型请求 + 执行工具
		stepEnd := a.step()

		a.session.append("step/end", fmt.Sprintf("step %d", step))

		// stepEnd 非 nil（completed）→ turn 结束；nil → 工具结果要再问模型 → 下一个 step
		if stepEnd != nil {
			a.session.append("turn/end", "completed")
			return false
		}
	}
}

func main() {
	fmt.Println("=== agent-loop：turn/step 状态机完整事件流 ===\n")

	a := &Agent{
		session: &Session{},
		llm: &FakeLLM{replies: []Message{
			// 第 1 次调用：调工具读文件
			{Role: "assistant", Text: "我先读文件", ToolCalls: []ToolCall{{Name: "read", Args: map[string]string{}}}},
			// 第 2 次调用：调工具执行命令
			{Role: "assistant", Text: "再执行命令", ToolCalls: []ToolCall{{Name: "bash", Args: map[string]string{}}}},
			// 第 3 次调用：纯文本完成
			{Role: "assistant", Text: "完成了"},
		}},
		inbox: []Message{{Role: "user", Text: "帮我处理这个任务"}},
	}

	// kick：外层 while(turn) 驱动（对应 agent.ts:210-223）
	a.turn()

	fmt.Println("\n=== 关键点 ===")
	fmt.Println("1. turn 是外层循环（一次任务），step 是内层循环（问一次模型 + 执行工具）")
	fmt.Println("2. 每个动作都落日志（assistant/chunk 流式分片、tool/call、tool/result 成对）")
	fmt.Println("3. 模型不再调工具 → step 返回 completed → turn 结束")
	fmt.Println("4. deriveMessages 从日志投影历史（model-visible ⟺ logged）")

	fmt.Println("\n=== deriveMessages 投影出的模型历史 ===")
	for _, m := range a.deriveMessages() {
		fmt.Printf("  [%s] %s\n", m.Role, m.Text)
	}
}
