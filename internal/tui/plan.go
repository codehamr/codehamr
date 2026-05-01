package tui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/plan"
)

// planStallLimit caps how many consecutive stream-closes the executor will
// nudge on the same active task (or wait for submit_plan) before yielding
// control. Prevents runaway auto-loops when the agent is genuinely stuck.
const planStallLimit = 5

// planPhase tracks where plan mode is in its lifecycle. None is the default —
// no plan is active and the session behaves like regular chat. The rest are
// the three coarse steps: plan the work, run the tasks, write the wrap-up.
type planPhase int

const (
	planPhaseNone planPhase = iota
	planPhasePlanning
	planPhaseExecuting
	planPhaseWrapUp
)

// planTriggerRE matches user messages that open with an imperative plan
// keyword — "plan", "plane", or "planen" as a standalone word. Case-
// insensitive. Does NOT match "planet", "planning", "plans", etc.
var planTriggerRE = regexp.MustCompile(`(?i)^\s*plan(e|en)?\b`)

// startsWithPlanKeyword reports whether the user message opens with a
// recognized plan-mode trigger.
func startsWithPlanKeyword(s string) bool {
	return planTriggerRE.MatchString(s)
}

// planningDirective is prepended to the user's plan-trigger message to
// give local models a hard instruction they can't misread as a heuristic.
// Qwen-class backends ignore "should I plan this?" soft rules; this
// converts the decision into a direct order.
const planningDirective = "[codehamr: plan-mode triggered. Produce a flat, typed plan and call `submit_plan` exactly once with every task. Each task declares its `inputs` (backward refs to earlier tasks' `outputs`) and its `outputs` (structured data it produces). Do not start working on any task yourself — after submit_plan is accepted, the executor starts task 01 with fresh context.]"

// resetPlanState clears every field that plan-mode bookkeeping writes, so
// /clear and post-plan transitions don't leak state into the next turn.
func (m *Model) resetPlanState() {
	m.plan = nil
	m.planPhase = planPhaseNone
	m.planActiveNum = ""
	m.planStallTurns = 0
	m.planWrapUpStarted = false
	m.planMisses = 0
}

// planAdvance runs at every stream close and deterministically decides what
// the executor needs to do next based on m.planPhase + m.plan state. Returns
// a tea.Cmd that initiates a new chat turn, or nil when the executor yields
// control back to the user.
//
// Phase contract:
//
//	None       → no plan active; returns nil.
//	Planning   → waiting for submit_plan to land. If m.plan has been set by
//	             the tool handler, transitions to Executing and fresh-starts
//	             task 01. If not, nudge (up to planStallLimit), then yield.
//	Executing  → inspects m.plan:
//	             - AllDone + wrap-up not yet started → trigger wrap-up turn.
//	             - AllDone + wrap-up done            → finish, reset state.
//	             - no Active task (just completed)   → promote first Open,
//	                                                   fresh-start.
//	             - Active still in progress          → stall nudge or yield.
//	WrapUp     → the wrap-up turn just closed; finalize + reset state.
//
// Any Cmd this returns leaves phase=phaseThinking with a fresh turnCtx, same
// contract as submit(), so Ctrl+C cancels cleanly.
func (m *Model) planAdvance() tea.Cmd {
	switch m.planPhase {
	case planPhaseNone:
		return nil

	case planPhasePlanning:
		if m.plan == nil {
			m.planStallTurns++
			if m.planStallTurns > planStallLimit {
				m.appendLine(styleWarn.Render(fmt.Sprintf(
					"⚠ plan: no submit_plan after %d turns — cancelling plan mode. Type to continue.",
					m.planStallTurns)))
				m.resetPlanState()
				return nil
			}
			return m.planNudgePlanning()
		}
		// Plan accepted — promote task 01 and fresh-start.
		m.planPhase = planPhaseExecuting
		return m.planPromoteAndStart(&m.plan.Tasks[0])

	case planPhaseExecuting:
		if m.plan == nil {
			m.resetPlanState()
			return nil
		}
		if m.plan.AllDone() {
			if m.planWrapUpStarted {
				// wrap-up turn already ran (shouldn't re-enter Executing
				// after that — defensive).
				m.appendLine(styleOK.Render("✓ plan done"))
				m.resetPlanState()
				return nil
			}
			m.planWrapUpStarted = true
			m.planPhase = planPhaseWrapUp
			m.planStallTurns = 0
			m.planActiveNum = ""
			return m.planTriggerWrapUp()
		}
		active := m.plan.Active()
		if active == nil {
			// complete_task just fired and there's still open work —
			// promote the next task and fresh-start.
			firstOpen := m.plan.FirstOpen()
			if firstOpen == nil {
				// Shouldn't reach here (AllDone would have handled it),
				// but be defensive.
				m.resetPlanState()
				return nil
			}
			return m.planPromoteAndStart(firstOpen)
		}
		// Same active task still in progress — agent didn't call
		// complete_task. Nudge up to planStallLimit, then yield.
		if m.planActiveNum != active.Number {
			// Executor's view of active task drifted — re-seed and start fresh.
			m.planActiveNum = active.Number
			m.planStallTurns = 0
			return m.planStartFreshTask(active.Number)
		}
		m.planStallTurns++
		if m.planStallTurns > planStallLimit {
			m.appendLine(styleWarn.Render(fmt.Sprintf(
				"⚠ plan: task %s stalled after %d turns — stopping auto-loop. Type to continue.",
				active.Number, m.planStallTurns)))
			m.planStallTurns = 0
			return nil
		}
		return m.planNudgeTask(active)

	case planPhaseWrapUp:
		m.appendLine(styleOK.Render("✓ plan done"))
		m.resetPlanState()
		return nil
	}
	return nil
}

// planPromoteAndStart marks t Active, records it as the executor's current
// task, emits the inline banner, and returns the fresh-start Cmd. Unifies
// the two promotion paths (initial task 01 and every later task after
// complete_task) so bannering, stall-reset, and MarkActive-failure handling
// stay identical. planPhase is set by the caller — this is pure promotion.
func (m *Model) planPromoteAndStart(t *plan.Task) tea.Cmd {
	if err := m.plan.MarkActive(t.Number); err != nil {
		m.appendLine(styleError.Render("✗ plan: " + err.Error()))
		m.resetPlanState()
		return nil
	}
	m.planActiveNum = t.Number
	m.planStallTurns = 0
	m.appendLine(styleOK.Render(fmt.Sprintf("▶ Task %s: %s", t.Number, t.Title)))
	return m.planStartFreshTask(t.Number)
}

// planStartFreshTask wipes history and seeds a new turn with the active
// task's rendered context: plan overview, the task's brief, resolved
// inputs, output contract, and workflow instructions. This IS the context
// — the only thing the agent carries from prior tasks is the resolved
// input values, by design.
func (m *Model) planStartFreshTask(number string) tea.Cmd {
	ctxStr, err := m.plan.RenderTaskContext(number)
	if err != nil {
		m.appendLine(styleError.Render("✗ plan: " + err.Error()))
		return nil
	}
	return m.freshUserTurn(ctxStr)
}

// planNudgeTask appends a short user-role reminder when the agent stalled
// on an active task without calling complete_task. Terse — nudges shouldn't
// eat context.
func (m *Model) planNudgeTask(active *plan.Task) tea.Cmd {
	missing := ""
	if err := active.OutputsComplete(); err != nil {
		missing = " (" + err.Error() + ")"
	}
	return m.appendUserTurn(fmt.Sprintf(
		"Continue task %s. Record remaining declared outputs via set_task_output%s, then call complete_task. If genuinely blocked, set_task_output(name=\"summary\", value=\"Blocked: <reason>\") and still call complete_task.",
		active.Number, missing))
}

// planNudgePlanning prods the agent to call submit_plan when the planning
// turn ended without a plan arriving.
func (m *Model) planNudgePlanning() tea.Cmd {
	return m.appendUserTurn("You haven't called submit_plan yet. Call it now with the full typed plan (problem, tasks, each task's inputs/outputs). Exactly one submit_plan call per planning turn.")
}

// planTriggerWrapUp fires the final summary turn after all tasks are done.
// Seeded with a fresh context so the agent's summary draws from the plan's
// recorded outputs, not any leftover noise from the last task.
func (m *Model) planTriggerWrapUp() tea.Cmd {
	m.appendLine(styleOK.Render("✓ All tasks done — writing final summary"))
	var b strings.Builder
	b.WriteString(m.plan.RenderOverview())
	b.WriteString("\nAll tasks complete. Task summaries:\n")
	for _, t := range m.plan.Tasks {
		if v, ok := t.Results["summary"]; ok {
			// Same rationale as RenderTaskContext: one bloated summary would
			// otherwise swallow the wrap-up turn's budget.
			fmt.Fprintf(&b, "\n- **task %s · %s:** %s", t.Number, t.Title, chmctx.Truncate(v))
		}
	}
	b.WriteString("\n\nReply with one clean paragraph (3-6 sentences) describing what was built, key decisions, and any loose ends. That reply is the only record of this plan — there is no plan file. Do not call any plan tools; just write the paragraph and end the turn.")
	return m.freshUserTurn(b.String())
}

// requireActiveTask is the shared precondition for set_task_output and
// complete_task: a plan must exist, the executor must be in the executing
// phase, and a task must be marked Active. Returns either the task or the
// rejection string (empty when the precondition holds). Tool name is
// embedded in the rejection so the agent sees which call was rejected.
func (m *Model) requireActiveTask(toolName string) (*plan.Task, string) {
	if m.plan == nil || m.planPhase != planPhaseExecuting {
		return nil, toolName + " rejected: no task is currently executing."
	}
	active := m.plan.Active()
	if active == nil {
		return nil, toolName + " rejected: no active task."
	}
	return active, ""
}

// handlePlanTool resolves a plan-tool call against in-memory state and
// returns the string payload that becomes the tool-role result message.
// All mutations happen synchronously on the UI goroutine — no goroutine
// races with concurrent stream events are possible.
func (m *Model) handlePlanTool(call chmctx.ToolCall) string {
	switch call.Name {
	case plan.ToolSubmitPlan:
		if m.planPhase != planPhasePlanning {
			return "submit_plan rejected: not in planning phase. If you need to replan, the user must /clear first."
		}
		if m.plan != nil {
			return "submit_plan rejected: a plan already exists for this session. Exactly one plan per session."
		}
		p, err := plan.NewFromArgs(call.Arguments)
		if err != nil {
			return "plan rejected: " + err.Error() + ". Fix and call submit_plan again."
		}
		m.plan = p
		m.planStallTurns = 0
		return fmt.Sprintf("plan accepted: %d tasks. End this turn — the executor will start task 01 with fresh context.", p.Total())

	case plan.ToolSetTaskOutput:
		active, rej := m.requireActiveTask(plan.ToolSetTaskOutput)
		if rej != "" {
			return rej
		}
		name := argString(call.Arguments["name"])
		value := argString(call.Arguments["value"])
		if err := active.SetOutput(name, value); err != nil {
			return "set_task_output rejected: " + err.Error()
		}
		return fmt.Sprintf("output %q recorded for task %s (%d/%d declared outputs set).",
			name, active.Number, len(active.Results), len(active.Outputs))

	case plan.ToolCompleteTask:
		active, rej := m.requireActiveTask(plan.ToolCompleteTask)
		if rej != "" {
			return rej
		}
		if err := active.OutputsComplete(); err != nil {
			return "complete_task rejected: " + err.Error() + ". Call set_task_output for the missing output first."
		}
		num := active.Number
		if err := m.plan.MarkDone(num); err != nil {
			return "complete_task rejected: " + err.Error()
		}
		m.planStallTurns = 0
		return fmt.Sprintf("task %s complete. End the turn — the executor advances next.", num)

	case plan.ToolGetTaskOutput:
		if m.plan == nil {
			return "get_task_output rejected: no plan is loaded."
		}
		taskNum := argString(call.Arguments["task"])
		name := argString(call.Arguments["output"])
		val, ok := m.plan.GetOutput(taskNum, name)
		if !ok {
			return fmt.Sprintf("get_task_output: task %s has no recorded output %q.", taskNum, name)
		}
		m.planMisses++
		// Mirror the bash/MCP tool-result truncation: a stored output the
		// size of the context window would otherwise blow past the budget
		// on retrieval.
		return chmctx.Truncate(val)
	}
	return "unknown plan tool: " + call.Name
}

// argString coerces a tool-call argument into its string form. The LLM is
// contractually supposed to send strings for string-typed args, but some
// backends relay numbers / booleans / even small objects verbatim; silently
// returning "" for those via a plain type assertion turned out to drop real
// values. Strings pass through; nil returns ""; everything else goes through
// json.Marshal so structured data survives as a valid JSON string (falling
// back to fmt.Sprint if the value can't be marshalled, which only happens
// for exotic types like channels that can't appear on a JSON wire anyway).
func argString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprint(v)
}
