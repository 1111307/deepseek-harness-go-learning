package main

// deepseek-harness Human-in-the-loop 完整复现（含 waterfall 链 / policy 持久化 / 询问型 / 多会话 scope）
// 章节：human-in-the-loop
//
// 对照真实源码：
//   user-approval/src/index.ts  → approval/request waterfall + policy + 审计 + fail-closed
//   user-questions/src/index.ts → ask()（询问型 HITL，provider 挂起）
//   core/scope/src/index.ts     → scopeTarget（按 agent 路由，多会话隔离）
//   api-proxy.ts / approvals.ts → 挂起表 + 双通道
//
// 运行：go run main.go

import (
	"fmt"
	"time"
)

// ============ 基础类型 ============

type Outcome string

const (
	AllowedOnce Outcome = "allowed-once"
	Rejected    Outcome = "rejected"
	Cancelled   Outcome = "cancelled"
	Unavailable Outcome = "unavailable"
)

var outcomeWhitelist = map[Outcome]bool{AllowedOnce: true, Rejected: true, Cancelled: true, Unavailable: true}

type ApprovalPolicy string

const (
	PolicyAsk   ApprovalPolicy = "ask"
	PolicyNever ApprovalPolicy = "never"
)

type ToolCall struct {
	Name string
	Args map[string]string
}

type Message struct {
	Role      string
	Text      string
	ToolCalls []ToolCall
}

type Event struct {
	Seq  int
	Type string
	Data string
}

// ============ Session：policy 走 durable 日志（不存内存字段）============

type Session struct {
	events   []Event
	openTurn bool
}

func (s *Session) appendEvent(t string, data string) {
	s.events = append(s.events, Event{Seq: len(s.events), Type: t, Data: data})
}

// setPolicy 对应 user-approval/src/index.ts:142-147：append approval/policy（durable）
func (s *Session) setPolicy(policy ApprovalPolicy) {
	s.appendEvent("approval/policy", string(policy))
}

// effectivePolicy 对应 index.ts:112-118：从日志尾部回放最后一个 approval/policy
func (s *Session) effectivePolicy() ApprovalPolicy {
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.events[i].Type == "approval/policy" {
			return ApprovalPolicy(s.events[i].Data)
		}
	}
	return PolicyAsk // 默认
}

// ============ Agent：作用域（id）+ answerer 链 + 询问 provider ============

// Answerer 是 approval/request waterfall 的一环。
// 返回 (outcome, handled)：handled=true 短路给出决定；handled=false 委托给下一环。
type Answerer func(req ApprovalRequest) (Outcome, bool)

// UserQuestionProvider 是询问型 HITL 的 UI 侧：挂起等答案。
type UserQuestionProvider func(questions []Question) Answer

type Agent struct {
	id               string
	session          *Session
	answerers        []Answerer           // waterfall 链
	questionProvider UserQuestionProvider // 询问型 provider
	userDecision     Outcome              // 模拟 UI answerer 这次返回的决定
}

// ============ 审批挂起表（按 agent.id 隔离，对应 scopeTarget 路由）============

var pending = map[string]chan Outcome{} // key = agent.id

type ApprovalRequest struct {
	agent    *Agent
	toolName string
	signal   chan struct{}
	aborted  bool
}

// ============ waterfall 链（对应 approval/request waterfall）============

// waterfall 遍历 answerer 链：第一个 handled 的胜出；全委托 → unavailable（fail-closed）。
// 对应 index.ts:317-328 的 ctx.waterfall(...) + 归一化。
func waterfall(answerers []Answerer, req ApprovalRequest) Outcome {
	for _, a := range answerers {
		if outcome, handled := a(req); handled {
			if !outcomeWhitelist[outcome] {
				return Unavailable // 归一化
			}
			return outcome
		}
	}
	return Unavailable // 全委托 → fail-closed
}

// ============ 审批（approval.request）============

func (s *Session) approvalRequest(req ApprovalRequest) Outcome {
	// ① hasOpenTurn 检查（index.ts:259-265）
	if !s.openTurn {
		panic("approval.request() outside an open turn")
	}
	id := fmt.Sprintf("q%d", time.Now().UnixNano())

	// ② 审计：approval/asked
	s.appendEvent("approval/asked", fmt.Sprintf("id=%s agent=%s tool=%s", id, req.agent.id, req.toolName))

	// ③ decide
	outcome := s.decide(req)

	// ④ 审计：approval/decided
	s.appendEvent("approval/decided", fmt.Sprintf("id=%s outcome=%s", id, outcome))

	return outcome
}

func (s *Session) decide(req ApprovalRequest) Outcome {
	if req.aborted {
		return Cancelled
	}
	// policy 从日志回放（不再内存字段）
	if s.effectivePolicy() == PolicyNever {
		return Rejected
	}
	return waterfall(req.agent.answerers, req)
}

// uiAnswerer 是 waterfall 链里挂起等用户的那一环（对应 UI answerer）。
func uiAnswerer(req ApprovalRequest) (Outcome, bool) {
	id := fmt.Sprintf("q%d", time.Now().UnixNano())
	ch := make(chan Outcome)
	pending[req.agent.id] = ch // 按 agent.id 隔离

	fmt.Printf("    [harness] ↓ 下行推送审批问题 (rpcId=%s, agent=%s): 工具 %q 需要审批\n", id, req.agent.id, req.toolName)

	// 模拟前端：收到 SSE，用户点 accept/reject，POST /api/respond
	go func() {
		time.Sleep(200 * time.Millisecond)
		respond(req.agent.id, id, req.agent.userDecision)
	}()

	select {
	case outcome := <-ch:
		delete(pending, req.agent.id)
		return outcome, true
	case <-req.signal:
		delete(pending, req.agent.id)
		return Cancelled, true
	}
}

// respond 对应 POST /api/respond：按 agent.id + rpcId 命中挂起
func respond(agentID, rpcId string, outcome Outcome) bool {
	if ch, ok := pending[agentID]; ok {
		ch <- outcome
		return true
	}
	return false
}

// ============ 询问型 HITL（ask_user_question）============

type Question struct {
	ID       string
	Question string
	Options  []string
}

type Answer struct {
	Selected []string
}

// askUserQuestion 对应 user-questions/src/index.ts:92-139
func (s *Session) askUserQuestion(questions []Question, provider UserQuestionProvider) Answer {
	if len(questions) == 0 {
		panic("EMPTY_QUESTIONS")
	}
	if provider == nil {
		panic("NO_PROVIDER") // fail-closed
	}
	// 挂起等 UI provider 返回答案
	return provider(questions)
}

// ============ 工具执行 + agent loop ============

func executeTool(name string) string {
	return name + " 执行结果"
}

type FakeLLM struct {
	replies []Message
	step    int
}

func (f *FakeLLM) Chat() Message {
	if f.step < len(f.replies) {
		m := f.replies[f.step]
		f.step++
		return m
	}
	return Message{Role: "assistant", Text: "完成"}
}

// runTask agent loop：区分询问型工具和普通工具
func runTask(agent *Agent, llm *FakeLLM) {
	s := agent.session
	s.openTurn = true
	s.appendEvent("turn/start", "turn 1")

	for {
		msg := llm.Chat()

		if len(msg.ToolCalls) == 0 {
			s.openTurn = false
			s.appendEvent("turn/end", "completed")
			fmt.Printf("    [%s] 模型回复：%q → turn/end\n", agent.id, msg.Text)
			return
		}

		for _, call := range msg.ToolCalls {
			if call.Name == "ask_user_question" {
				// 询问型：模型主动问人，挂起等答案，答案作为 tool result
				questions := []Question{{ID: "q1", Question: "继续执行还是停下？", Options: []string{"继续", "停下"}}}
				ans := s.askUserQuestion(questions, agent.questionProvider)
				fmt.Printf("    [%s] 询问用户 → 用户选择 %v → 作为 tool result 喂回\n", agent.id, ans.Selected)
			} else {
				// 审批型：工具执行前走 approval 门禁
				outcome := s.approvalRequest(ApprovalRequest{
					agent:    agent,
					toolName: call.Name,
					signal:   make(chan struct{}),
				})
				if outcome != AllowedOnce {
					fmt.Printf("    [%s] 审批结果 %s → 拒绝执行 %q\n", agent.id, outcome, call.Name)
					continue
				}
				fmt.Printf("    [%s] 审批通过 → 执行 %q → %s\n", agent.id, call.Name, executeTool(call.Name))
			}
		}
	}
}

func main() {
	// ===== 演示 1：waterfall 链（白名单规则短路 + UI answerer 委托）=====
	fmt.Println("=== 演示 1：waterfall 链（read 自动放行，bash 委托给 UI 问人）===")
	whitelistAnswerer := func(req ApprovalRequest) (Outcome, bool) {
		if req.toolName == "read" {
			return AllowedOnce, true // read 只读，自动放行（短路，不问人）
		}
		return "", false // 其他工具委托给下一环
	}
	s1 := &Session{}
	a1 := &Agent{
		id:           "agent-A",
		session:      s1,
		answerers:    []Answerer{whitelistAnswerer, uiAnswerer},
		userDecision: AllowedOnce,
	}
	llm1 := &FakeLLM{replies: []Message{
		{Role: "assistant", Text: "先读文件", ToolCalls: []ToolCall{{Name: "read", Args: map[string]string{}}}},
		{Role: "assistant", Text: "再执行命令", ToolCalls: []ToolCall{{Name: "bash", Args: map[string]string{}}}},
		{Role: "assistant", Text: "完成"},
	}}
	runTask(a1, llm1)

	// ===== 演示 2：policy 持久化（setPolicy → effectivePolicy 回放）=====
	fmt.Println("\n=== 演示 2：policy 持久化（从日志回放，而非内存字段）===")
	s2 := &Session{}
	s2.setPolicy(PolicyNever) // durable：append approval/policy
	a2 := &Agent{id: "agent-B", session: s2, answerers: []Answerer{uiAnswerer}, userDecision: AllowedOnce}
	llm2 := &FakeLLM{replies: []Message{
		{Role: "assistant", Text: "执行命令", ToolCalls: []ToolCall{{Name: "bash", Args: map[string]string{}}}},
		{Role: "assistant", Text: "被拒绝"},
	}}
	runTask(a2, llm2)
	fmt.Printf("    [说明] effectivePolicy 从日志回放 = %s（日志里最后一个 approval/policy）\n", s2.effectivePolicy())

	// ===== 演示 3：询问型（ask_user_question，模型主动问人）=====
	fmt.Println("\n=== 演示 3：询问型 HITL（ask_user_question 工具）===")
	s3 := &Session{}
	a3 := &Agent{
		id:      "agent-C",
		session: s3,
		questionProvider: func(qs []Question) Answer {
			fmt.Printf("    [harness] 挂起等用户回答：%q\n", qs[0].Question)
			time.Sleep(200 * time.Millisecond)
			return Answer{Selected: []string{"继续"}}
		},
	}
	llm3 := &FakeLLM{replies: []Message{
		{Role: "assistant", Text: "我需要确认一下", ToolCalls: []ToolCall{{Name: "ask_user_question", Args: map[string]string{}}}},
		{Role: "assistant", Text: "按用户选择继续，完成"},
	}}
	runTask(a3, llm3)

	// ===== 演示 4：多会话 scope 隔离（两个 agent 并发，审批各回各的）=====
	fmt.Println("\n=== 演示 4：多会话 scope 隔离（两个 agent 并发审批不串扰）===")
	sA := &Session{}
	sB := &Session{}
	agentA := &Agent{id: "agent-A", session: sA, answerers: []Answerer{uiAnswerer}, userDecision: AllowedOnce}
	agentB := &Agent{id: "agent-B", session: sB, answerers: []Answerer{uiAnswerer}, userDecision: Rejected}
	llmA := &FakeLLM{replies: []Message{
		{Role: "assistant", Text: "A 执行命令", ToolCalls: []ToolCall{{Name: "bash", Args: map[string]string{}}}},
		{Role: "assistant", Text: "A 完成"},
	}}
	llmB := &FakeLLM{replies: []Message{
		{Role: "assistant", Text: "B 写文件", ToolCalls: []ToolCall{{Name: "write", Args: map[string]string{}}}},
		{Role: "assistant", Text: "B 被拒"},
	}}
	done := make(chan struct{}, 2)
	go func() { runTask(agentA, llmA); done <- struct{}{} }()
	go func() { runTask(agentB, llmB); done <- struct{}{} }()
	<-done
	<-done
	fmt.Println("    [说明] pending 表按 agent.id 隔离，A 的 allowed 不影响 B 的 rejected")

	// ===== 审计日志（policy + asked/decided 都在日志里，可重放）=====
	fmt.Println("\n=== agent-B 的审计日志（policy + asked/decided 都是 durable 事件）===")
	for _, e := range s2.events {
		fmt.Printf("  seq=%d %s: %s\n", e.Seq, e.Type, e.Data)
	}
}
