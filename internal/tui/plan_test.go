package tui

import (
	"context"
	"net/http"
	"strings"
	"testing"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
	"github.com/codehamr/codehamr/internal/llm"
	"github.com/codehamr/codehamr/internal/plan"
)

// planTestModel wires a Model against a stubbed SSE server. The planAdvance /
// tool-handler tests don't exercise the HTTP stream — they probe state
// machine + tool handlers directly — but Model construction needs a server URL.
func planTestModel(t *testing.T) Model {
	t.Helper()
	return newTestModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	})
}

// samplePlan builds a small valid plan for tests. Three tasks with a chain
// of declared inputs/outputs mirroring a real decomposition.
func samplePlan(t *testing.T) *plan.Plan {
	t.Helper()
	p, err := plan.NewFromArgs(map[string]any{
		"problem": "Unit test plan.",
		"intro":   "Three tasks with declared I/O.",
		"tasks": []any{
			map[string]any{
				"number":  "01",
				"title":   "First",
				"details": "Do first thing.",
				"outputs": []any{
					map[string]any{"name": "file_list", "description": "files"},
					map[string]any{"name": "summary", "description": "handoff"},
				},
			},
			map[string]any{
				"number":  "02",
				"title":   "Second",
				"details": "Do second thing.",
				"inputs": []any{
					map[string]any{"task": "01", "output": "file_list"},
				},
				"outputs": []any{
					map[string]any{"name": "summary", "description": "handoff"},
				},
			},
			map[string]any{
				"number":  "03",
				"title":   "Third",
				"details": "Wrap it up.",
				"outputs": []any{
					map[string]any{"name": "summary", "description": "handoff"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("samplePlan: %v", err)
	}
	return p
}

func TestStartsWithPlanKeyword(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"plan mir ein CRM", true},
		{"Plan das ganze", true},
		{"plane die Aufgabe", true},
		{"planen wir das", true},
		{"plan me a backend", true},
		{"Plan: build X", true},
		{"PLAN this", true},
		{"   plan   leading whitespace", true},
		{"plan", true},
		{"planet earth", false},
		{"planning is hard", false},
		{"plans are good", false},
		{"planetary", false},
		{"refactor X", false},
		{"", false},
	} {
		got := startsWithPlanKeyword(tc.in)
		if got != tc.want {
			t.Errorf("startsWithPlanKeyword(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPlanAdvanceNoneIsIdle(t *testing.T) {
	m := planTestModel(t)
	if cmd := m.planAdvance(); cmd != nil {
		t.Errorf("planPhaseNone → expected nil Cmd, got non-nil")
	}
}

func TestPlanAdvancePlanningAcceptedPromotesFirst(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhasePlanning
	m.plan = samplePlan(t)

	cmd := m.planAdvance()
	if cmd == nil {
		t.Fatal("expected fresh-start Cmd for task 01, got nil")
	}
	if m.planPhase != planPhaseExecuting {
		t.Errorf("planPhase = %v, want executing", m.planPhase)
	}
	if m.planActiveNum != "01" {
		t.Errorf("planActiveNum = %q, want 01", m.planActiveNum)
	}
	if m.plan.Tasks[0].Status != plan.StatusActive {
		t.Errorf("task 01 status = %v", m.plan.Tasks[0].Status)
	}
	if len(m.history) != 1 {
		t.Fatalf("history len = %d, want 1 (fresh-started)", len(m.history))
	}
	if !strings.Contains(m.history[0].Content, "Task 01: First") {
		t.Errorf("fresh context missing Task 01 marker:\n%s", m.history[0].Content)
	}
}

func TestPlanAdvancePlanningStallsThenYields(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhasePlanning

	for i := 1; i <= planStallLimit; i++ {
		cmd := m.planAdvance()
		if cmd == nil {
			t.Fatalf("turn %d: expected nudge Cmd, got nil", i)
		}
		if m.planStallTurns != i {
			t.Errorf("turn %d: planStallTurns = %d, want %d", i, m.planStallTurns, i)
		}
	}
	// One more → yields and resets.
	if cmd := m.planAdvance(); cmd != nil {
		t.Errorf("over limit → expected nil Cmd, got non-nil")
	}
	if m.planPhase != planPhaseNone {
		t.Errorf("planPhase after yield = %v, want none", m.planPhase)
	}
}

func TestPlanAdvanceExecutingAdvancesToNextTask(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhaseExecuting
	m.plan = samplePlan(t)
	// simulate task 01 completed
	m.plan.Tasks[0].Status = plan.StatusDone
	m.plan.Tasks[0].Results = map[string]string{
		"file_list": "a.go\nb.go",
		"summary":   "ok",
	}
	m.planActiveNum = "01"

	cmd := m.planAdvance()
	if cmd == nil {
		t.Fatal("expected fresh-start Cmd for task 02, got nil")
	}
	if m.planActiveNum != "02" {
		t.Errorf("planActiveNum = %q, want 02", m.planActiveNum)
	}
	if m.plan.Tasks[1].Status != plan.StatusActive {
		t.Errorf("task 02 status = %v", m.plan.Tasks[1].Status)
	}
	if len(m.history) != 1 {
		t.Fatalf("history should be wiped to 1 seed, got %d", len(m.history))
	}
	// Task 02 declared an input on task 01 — resolved value must be present.
	if !strings.Contains(m.history[0].Content, "a.go") {
		t.Errorf("resolved input missing in fresh context:\n%s", m.history[0].Content)
	}
}

func TestPlanAdvanceExecutingAllDoneTriggersWrapUp(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhaseExecuting
	m.plan = samplePlan(t)
	for i := range m.plan.Tasks {
		m.plan.Tasks[i].Status = plan.StatusDone
		m.plan.Tasks[i].Results = map[string]string{"summary": "done"}
		if i == 0 {
			m.plan.Tasks[i].Results["file_list"] = "x"
		}
	}

	cmd := m.planAdvance()
	if cmd == nil {
		t.Fatal("expected wrap-up Cmd, got nil")
	}
	if m.planPhase != planPhaseWrapUp {
		t.Errorf("planPhase = %v, want wrapUp", m.planPhase)
	}
	if !m.planWrapUpStarted {
		t.Error("planWrapUpStarted should be true")
	}
	// Wrap-up context should mention the task summaries.
	if !strings.Contains(m.history[0].Content, "task summaries") && !strings.Contains(m.history[0].Content, "Task summaries") {
		t.Errorf("wrap-up context missing summaries heading:\n%s", m.history[0].Content)
	}
}

func TestPlanAdvanceExecutingStallsThenYields(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhaseExecuting
	m.plan = samplePlan(t)
	m.plan.Tasks[0].Status = plan.StatusActive
	m.planActiveNum = "01"

	for i := 1; i <= planStallLimit; i++ {
		cmd := m.planAdvance()
		if cmd == nil {
			t.Fatalf("turn %d: expected nudge Cmd, got nil", i)
		}
		if m.planStallTurns != i {
			t.Errorf("turn %d: planStallTurns = %d, want %d", i, m.planStallTurns, i)
		}
	}
	if cmd := m.planAdvance(); cmd != nil {
		t.Errorf("over limit → expected nil Cmd (yield), got non-nil")
	}
	if m.planStallTurns != 0 {
		t.Errorf("stall turns should reset after yield, got %d", m.planStallTurns)
	}
}

func TestPlanAdvanceWrapUpResetsOnClose(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhaseWrapUp
	m.plan = samplePlan(t)
	m.planWrapUpStarted = true

	if cmd := m.planAdvance(); cmd != nil {
		t.Errorf("wrap-up close → expected nil Cmd, got non-nil")
	}
	if m.planPhase != planPhaseNone {
		t.Errorf("planPhase = %v, want none", m.planPhase)
	}
	if m.plan != nil {
		t.Error("plan should be nil after wrap-up")
	}
}

func TestHandleSubmitPlanAccepts(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhasePlanning
	args := map[string]any{
		"problem": "Test.",
		"tasks": []any{
			map[string]any{
				"number":  "01",
				"title":   "Go",
				"details": "Do it.",
				"outputs": []any{map[string]any{"name": "summary", "description": "x"}},
			},
		},
	}
	result := m.handlePlanTool(chmctx.ToolCall{Name: plan.ToolSubmitPlan, Arguments: args})
	if !strings.Contains(result, "plan accepted") {
		t.Errorf("expected accept message, got: %q", result)
	}
	if m.plan == nil {
		t.Fatal("m.plan should be set")
	}
	if m.plan.Total() != 1 {
		t.Errorf("Total = %d", m.plan.Total())
	}
}

func TestHandleSubmitPlanRejectsWhenNotPlanning(t *testing.T) {
	m := planTestModel(t)
	// phase is None
	result := m.handlePlanTool(chmctx.ToolCall{
		Name:      plan.ToolSubmitPlan,
		Arguments: map[string]any{"problem": "x", "tasks": []any{}},
	})
	if !strings.Contains(result, "not in planning phase") {
		t.Errorf("expected phase rejection, got: %q", result)
	}
}

func TestHandleSubmitPlanRejectsDuplicate(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhasePlanning
	m.plan = samplePlan(t)
	result := m.handlePlanTool(chmctx.ToolCall{
		Name:      plan.ToolSubmitPlan,
		Arguments: map[string]any{"problem": "x", "tasks": []any{}},
	})
	if !strings.Contains(result, "already exists") {
		t.Errorf("expected duplicate rejection, got: %q", result)
	}
}

func TestHandleSubmitPlanRejectsInvalid(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhasePlanning
	args := map[string]any{
		"problem": "x",
		"tasks": []any{
			map[string]any{"number": "1", "title": "bad", "details": "x", "outputs": []any{
				map[string]any{"name": "summary", "description": "x"},
			}},
		},
	}
	result := m.handlePlanTool(chmctx.ToolCall{Name: plan.ToolSubmitPlan, Arguments: args})
	if !strings.Contains(result, "plan rejected") {
		t.Errorf("expected rejection for bad number format, got: %q", result)
	}
	if m.plan != nil {
		t.Error("m.plan should remain nil on rejection")
	}
}

func TestHandleSetTaskOutputRecords(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhaseExecuting
	m.plan = samplePlan(t)
	m.plan.Tasks[0].Status = plan.StatusActive

	res := m.handlePlanTool(chmctx.ToolCall{
		Name:      plan.ToolSetTaskOutput,
		Arguments: map[string]any{"name": "summary", "value": "ok."},
	})
	if !strings.Contains(res, "recorded for task 01") {
		t.Errorf("expected record message, got: %q", res)
	}
	if m.plan.Tasks[0].Results["summary"] != "ok." {
		t.Errorf("value not stored: %v", m.plan.Tasks[0].Results)
	}
}

// TestHandleSetTaskOutputCoercesNonStringValue: the LLM occasionally sends
// a non-string `value` despite the schema (numbers from buggy JSON modes,
// structured objects). Plain `.(string)` silently drops these to "", which
// then gets stored as empty output and propagates as phantom data downstream.
// argString should JSON-marshal structured values and fmt.Sprint primitives
// so the real payload survives.
func TestHandleSetTaskOutputCoercesNonStringValue(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhaseExecuting
	m.plan = samplePlan(t)
	m.plan.Tasks[0].Status = plan.StatusActive

	m.handlePlanTool(chmctx.ToolCall{
		Name:      plan.ToolSetTaskOutput,
		Arguments: map[string]any{"name": "summary", "value": float64(42)},
	})
	if got := m.plan.Tasks[0].Results["summary"]; got != "42" {
		t.Errorf("numeric value coerced wrong: got %q, want %q", got, "42")
	}

	m.handlePlanTool(chmctx.ToolCall{
		Name:      plan.ToolSetTaskOutput,
		Arguments: map[string]any{"name": "file_list", "value": []any{"a.go", "b.go"}},
	})
	if got := m.plan.Tasks[0].Results["file_list"]; got != `["a.go","b.go"]` {
		t.Errorf("array value coerced wrong: got %q", got)
	}
}

// TestHandleGetTaskOutputTruncatesHuge: a stored task output larger than
// ToolOutputCap must be truncated on retrieval so a buggy set_task_output
// with megabytes of content can't blow past the context budget the next
// time the agent asks for it.
func TestHandleGetTaskOutputTruncatesHuge(t *testing.T) {
	m := planTestModel(t)
	m.plan = samplePlan(t)
	m.plan.Tasks[0].Status = plan.StatusDone
	huge := strings.Repeat("x", 4*10000) // ~10k tokens, well over ToolOutputCap
	m.plan.Tasks[0].Results = map[string]string{
		"file_list": huge,
		"summary":   "ok",
	}

	res := m.handlePlanTool(chmctx.ToolCall{
		Name:      plan.ToolGetTaskOutput,
		Arguments: map[string]any{"task": "01", "output": "file_list"},
	})
	if !strings.Contains(res, "truncated") {
		t.Fatalf("large stored output should be truncated on retrieval: len=%d", len(res))
	}
	if len(res) >= len(huge) {
		t.Fatalf("truncation produced no shrinkage: got %d chars, want << %d", len(res), len(huge))
	}
}

func TestHandleSetTaskOutputRejectsUndeclared(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhaseExecuting
	m.plan = samplePlan(t)
	m.plan.Tasks[0].Status = plan.StatusActive

	res := m.handlePlanTool(chmctx.ToolCall{
		Name:      plan.ToolSetTaskOutput,
		Arguments: map[string]any{"name": "bogus", "value": "x"},
	})
	if !strings.Contains(res, "rejected") {
		t.Errorf("expected rejection for undeclared output, got: %q", res)
	}
}

func TestHandleCompleteTaskRequiresAllOutputs(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhaseExecuting
	m.plan = samplePlan(t)
	m.plan.Tasks[0].Status = plan.StatusActive
	// only set one of two declared outputs
	_ = m.plan.Tasks[0].SetOutput("file_list", "a.go")

	res := m.handlePlanTool(chmctx.ToolCall{Name: plan.ToolCompleteTask})
	if !strings.Contains(res, "rejected") {
		t.Errorf("expected rejection for missing summary, got: %q", res)
	}
	if m.plan.Tasks[0].Status != plan.StatusActive {
		t.Errorf("task should remain active, got %v", m.plan.Tasks[0].Status)
	}

	_ = m.plan.Tasks[0].SetOutput("summary", "done.")
	res2 := m.handlePlanTool(chmctx.ToolCall{Name: plan.ToolCompleteTask})
	if !strings.Contains(res2, "task 01 complete") {
		t.Errorf("expected completion message, got: %q", res2)
	}
	if m.plan.Tasks[0].Status != plan.StatusDone {
		t.Errorf("task 01 should be done, got %v", m.plan.Tasks[0].Status)
	}
}

func TestHandleGetTaskOutputReturnsValueAndBumpsMiss(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhaseExecuting
	m.plan = samplePlan(t)
	m.plan.Tasks[0].Status = plan.StatusDone
	_ = m.plan.Tasks[0].SetOutput("file_list", "a.go,b.go")
	_ = m.plan.Tasks[0].SetOutput("summary", "done")

	res := m.handlePlanTool(chmctx.ToolCall{
		Name:      plan.ToolGetTaskOutput,
		Arguments: map[string]any{"task": "01", "output": "file_list"},
	})
	if res != "a.go,b.go" {
		t.Errorf("get_task_output payload = %q", res)
	}
	if m.planMisses != 1 {
		t.Errorf("planMisses = %d, want 1", m.planMisses)
	}
}

func TestHandleGetTaskOutputUnknown(t *testing.T) {
	m := planTestModel(t)
	m.planPhase = planPhaseExecuting
	m.plan = samplePlan(t)

	res := m.handlePlanTool(chmctx.ToolCall{
		Name:      plan.ToolGetTaskOutput,
		Arguments: map[string]any{"task": "02", "output": "nope"},
	})
	if !strings.Contains(res, "no recorded output") {
		t.Errorf("expected missing-output message, got: %q", res)
	}
}

func TestResetPlanStateClearsEverything(t *testing.T) {
	m := planTestModel(t)
	m.plan = samplePlan(t)
	m.planPhase = planPhaseExecuting
	m.planActiveNum = "02"
	m.planStallTurns = 3
	m.planWrapUpStarted = true

	m.resetPlanState()

	if m.plan != nil || m.planPhase != planPhaseNone || m.planActiveNum != "" ||
		m.planStallTurns != 0 || m.planWrapUpStarted {
		t.Errorf("resetPlanState incomplete: %+v", m)
	}
}

func TestBuildToolsExposesPlanToolsByPhase(t *testing.T) {
	m := planTestModel(t)
	// default phase None → no plan tools
	names := toolNames(m.buildTools())
	if contains(names, plan.ToolSubmitPlan) {
		t.Error("none-phase should not expose submit_plan")
	}
	// planning phase → submit_plan present, execution tools absent
	m.planPhase = planPhasePlanning
	names = toolNames(m.buildTools())
	if !contains(names, plan.ToolSubmitPlan) {
		t.Error("planning-phase should expose submit_plan")
	}
	if contains(names, plan.ToolCompleteTask) {
		t.Error("planning-phase should NOT expose complete_task")
	}
	// executing phase → execution tools present, submit_plan gone
	m.planPhase = planPhaseExecuting
	names = toolNames(m.buildTools())
	for _, want := range []string{plan.ToolSetTaskOutput, plan.ToolCompleteTask, plan.ToolGetTaskOutput} {
		if !contains(names, want) {
			t.Errorf("executing-phase missing %s", want)
		}
	}
	if contains(names, plan.ToolSubmitPlan) {
		t.Error("executing-phase should NOT expose submit_plan")
	}
	// wrap-up phase → no plan tools
	m.planPhase = planPhaseWrapUp
	names = toolNames(m.buildTools())
	for _, forbidden := range []string{plan.ToolSubmitPlan, plan.ToolSetTaskOutput, plan.ToolCompleteTask, plan.ToolGetTaskOutput} {
		if contains(names, forbidden) {
			t.Errorf("wrap-up phase should NOT expose %s", forbidden)
		}
	}
}

// TestTurnCapBumpsPlanStallInPlanMode: when a plan is loaded, the intra-stream
// cap-abort branch must also increment planStallTurns so repeated drift on the
// same task is caught by the existing stall-limit logic.
func TestTurnCapBumpsPlanStallInPlanMode(t *testing.T) {
	m := planTestModel(t)
	m.plan = samplePlan(t)
	m.planPhase = planPhaseExecuting
	m.plan.Tasks[0].Status = plan.StatusActive
	m.planActiveNum = "01"
	m.planStallTurns = 2

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.turnCtx = ctx
	m.cancel = cancel
	m.phase = phaseThinking
	m.turnToolCalls = maxToolsPerTurn
	m.pending = []chmctx.ToolCall{{Name: "bash"}}

	out, _ := m.Update(streamClosedMsg{})
	om := out.(Model)

	if om.planStallTurns != 3 {
		t.Fatalf("cap in plan-mode should bump planStallTurns (2→3), got %d", om.planStallTurns)
	}
	if om.phase.active() {
		t.Fatalf("phase must be idle after cap abort, got %v", om.phase)
	}
}

func toolNames(ts []llm.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Function.Name)
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
