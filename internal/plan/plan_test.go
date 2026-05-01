package plan

import (
	"strings"
	"testing"
)

// samplePlanArgs is the canonical submit_plan payload used across tests.
// Three tasks with a realistic input chain: 02 depends on 01, 03 on both.
func samplePlanArgs() map[string]any {
	return map[string]any{
		"problem": "Build the thing.",
		"intro":   "Decompose into parser → validator → wirer.",
		"tasks": []any{
			map[string]any{
				"number":  "01",
				"title":   "Parse",
				"details": "Write parser with tests.",
				"outputs": []any{
					map[string]any{"name": "file_list", "description": "files touched"},
					map[string]any{"name": "summary", "description": "terse handoff"},
				},
			},
			map[string]any{
				"number":  "02",
				"title":   "Validate",
				"details": "Add validation rules.",
				"inputs": []any{
					map[string]any{"task": "01", "output": "file_list"},
				},
				"outputs": []any{
					map[string]any{"name": "rules", "description": "rule names"},
					map[string]any{"name": "summary", "description": "terse handoff"},
				},
			},
			map[string]any{
				"number":  "03",
				"title":   "Wire",
				"details": "Plug it into the app.",
				"inputs": []any{
					map[string]any{"task": "01", "output": "file_list"},
					map[string]any{"task": "02", "output": "rules"},
				},
				"outputs": []any{
					map[string]any{"name": "summary", "description": "terse handoff"},
				},
			},
		},
	}
}

func TestNewFromArgsHappy(t *testing.T) {
	p, err := NewFromArgs(samplePlanArgs())
	if err != nil {
		t.Fatalf("NewFromArgs: %v", err)
	}
	if p.Problem != "Build the thing." {
		t.Errorf("Problem = %q", p.Problem)
	}
	if len(p.Tasks) != 3 {
		t.Fatalf("Tasks len = %d, want 3", len(p.Tasks))
	}
	if p.Tasks[0].Number != "01" || p.Tasks[0].Title != "Parse" {
		t.Errorf("Task 0 = %+v", p.Tasks[0])
	}
	if len(p.Tasks[2].Inputs) != 2 {
		t.Errorf("Task 2 inputs = %v", p.Tasks[2].Inputs)
	}
	// Results maps must be initialized — SetOutput relies on it.
	for i, tk := range p.Tasks {
		if tk.Results == nil {
			t.Errorf("Task %d Results nil after construction", i)
		}
	}
}

func TestValidateEmptyProblem(t *testing.T) {
	args := samplePlanArgs()
	args["problem"] = "   "
	if _, err := NewFromArgs(args); err == nil {
		t.Fatal("expected error for empty problem")
	}
}

func TestValidateNoTasks(t *testing.T) {
	args := map[string]any{"problem": "x", "tasks": []any{}}
	if _, err := NewFromArgs(args); err == nil {
		t.Fatal("expected error for empty tasks")
	}
}

func TestValidateBadNumberFormat(t *testing.T) {
	for _, bad := range []string{"1", "001", "aa", "", "3"} {
		args := samplePlanArgs()
		args["tasks"].([]any)[0].(map[string]any)["number"] = bad
		if _, err := NewFromArgs(args); err == nil {
			t.Errorf("expected error for number %q", bad)
		}
	}
}

func TestValidateDuplicateNumber(t *testing.T) {
	args := samplePlanArgs()
	args["tasks"].([]any)[1].(map[string]any)["number"] = "01"
	if _, err := NewFromArgs(args); err == nil {
		t.Fatal("expected error for duplicate number")
	}
}

func TestValidateMissingTitle(t *testing.T) {
	args := samplePlanArgs()
	args["tasks"].([]any)[0].(map[string]any)["title"] = ""
	if _, err := NewFromArgs(args); err == nil {
		t.Fatal("expected error for empty title")
	}
}

func TestValidateMissingDetails(t *testing.T) {
	args := samplePlanArgs()
	args["tasks"].([]any)[0].(map[string]any)["details"] = ""
	if _, err := NewFromArgs(args); err == nil {
		t.Fatal("expected error for empty details")
	}
}

func TestValidateNoOutputs(t *testing.T) {
	args := samplePlanArgs()
	args["tasks"].([]any)[0].(map[string]any)["outputs"] = []any{}
	if _, err := NewFromArgs(args); err == nil {
		t.Fatal("expected error for task with no outputs")
	}
}

func TestValidateDuplicateOutputName(t *testing.T) {
	args := samplePlanArgs()
	args["tasks"].([]any)[0].(map[string]any)["outputs"] = []any{
		map[string]any{"name": "summary", "description": "a"},
		map[string]any{"name": "summary", "description": "b"},
	}
	if _, err := NewFromArgs(args); err == nil {
		t.Fatal("expected error for duplicate output name")
	}
}

func TestValidateForwardInputRef(t *testing.T) {
	args := samplePlanArgs()
	// Task 01 references task 02 — forward ref, illegal.
	args["tasks"].([]any)[0].(map[string]any)["inputs"] = []any{
		map[string]any{"task": "02", "output": "rules"},
	}
	if _, err := NewFromArgs(args); err == nil {
		t.Fatal("expected error for forward input ref")
	}
}

func TestValidateSelfInputRef(t *testing.T) {
	args := samplePlanArgs()
	args["tasks"].([]any)[1].(map[string]any)["inputs"] = []any{
		map[string]any{"task": "02", "output": "rules"},
	}
	if _, err := NewFromArgs(args); err == nil {
		t.Fatal("expected error for self input ref")
	}
}

func TestValidateUnknownInputTask(t *testing.T) {
	args := samplePlanArgs()
	args["tasks"].([]any)[1].(map[string]any)["inputs"] = []any{
		map[string]any{"task": "99", "output": "x"},
	}
	if _, err := NewFromArgs(args); err == nil {
		t.Fatal("expected error for unknown input task")
	}
}

func TestValidateUnknownInputOutput(t *testing.T) {
	args := samplePlanArgs()
	args["tasks"].([]any)[1].(map[string]any)["inputs"] = []any{
		map[string]any{"task": "01", "output": "not_declared"},
	}
	if _, err := NewFromArgs(args); err == nil {
		t.Fatal("expected error for unknown input output name")
	}
}

func TestFindActiveFirstOpen(t *testing.T) {
	p, _ := NewFromArgs(samplePlanArgs())
	p.Tasks[0].Status = StatusDone
	p.Tasks[1].Status = StatusActive
	if a := p.Active(); a == nil || a.Number != "02" {
		t.Errorf("Active = %v", a)
	}
	if fo := p.FirstOpen(); fo == nil || fo.Number != "03" {
		t.Errorf("FirstOpen = %v", fo)
	}
	if p.Find("03") == nil {
		t.Errorf("Find(03) = nil")
	}
	if p.Find("99") != nil {
		t.Errorf("Find(99) should be nil")
	}
}

func TestMarkActiveDoneAllDone(t *testing.T) {
	p, _ := NewFromArgs(samplePlanArgs())
	if err := p.MarkActive("01"); err != nil {
		t.Fatal(err)
	}
	if p.Tasks[0].Status != StatusActive {
		t.Errorf("Task 01 status = %v", p.Tasks[0].Status)
	}
	if err := p.MarkActive("01"); err == nil {
		t.Error("second MarkActive on same task should error")
	}
	_ = p.Tasks[0].SetOutput("file_list", "a.go")
	_ = p.Tasks[0].SetOutput("summary", "parsed.")
	if err := p.MarkDone("01"); err != nil {
		t.Fatal(err)
	}
	if p.Tasks[0].Status != StatusDone {
		t.Errorf("Task 01 status after MarkDone = %v", p.Tasks[0].Status)
	}
	if p.AllDone() {
		t.Error("AllDone should be false — tasks 02/03 still open")
	}
	if err := p.MarkDone("99"); err == nil {
		t.Error("MarkDone on unknown should error")
	}
	if err := p.MarkDone("02"); err == nil {
		t.Error("MarkDone on open task should error (wrong status)")
	}
}

func TestTotalReportsTaskCount(t *testing.T) {
	p, _ := NewFromArgs(samplePlanArgs())
	if p.Total() != 3 {
		t.Errorf("Total = %d", p.Total())
	}
}

func TestSetOutputGetOutputComplete(t *testing.T) {
	p, _ := NewFromArgs(samplePlanArgs())
	t01 := &p.Tasks[0]
	if err := t01.SetOutput("file_list", "a.go, b.go"); err != nil {
		t.Fatal(err)
	}
	// Undeclared name rejected.
	if err := t01.SetOutput("bogus", "x"); err == nil {
		t.Error("undeclared output name should be rejected")
	}
	// Outputs still incomplete (summary missing).
	if err := t01.OutputsComplete(); err == nil {
		t.Error("OutputsComplete should error when summary missing")
	}
	if err := t01.SetOutput("summary", "parsed OK"); err != nil {
		t.Fatal(err)
	}
	if err := t01.OutputsComplete(); err != nil {
		t.Errorf("OutputsComplete after both set: %v", err)
	}
	// GetOutput via plan.
	if v, ok := p.GetOutput("01", "file_list"); !ok || v != "a.go, b.go" {
		t.Errorf("GetOutput = (%q, %v)", v, ok)
	}
	if _, ok := p.GetOutput("99", "x"); ok {
		t.Error("GetOutput on unknown task should return false")
	}
	if _, ok := p.GetOutput("01", "missing"); ok {
		t.Error("GetOutput on unrecorded name should return false")
	}
}

func TestRenderOverviewShowsStatuses(t *testing.T) {
	p, _ := NewFromArgs(samplePlanArgs())
	p.Tasks[0].Status = StatusDone
	p.Tasks[1].Status = StatusActive
	out := p.RenderOverview()
	for _, want := range []string{
		"# Plan: Build the thing.",
		"Decompose into parser → validator → wirer.",
		"- [x] 01 Parse",
		"- [~] 02 Validate",
		"- [ ] 03 Wire",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderOverview missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderTaskContextResolvesInputs(t *testing.T) {
	p, _ := NewFromArgs(samplePlanArgs())
	p.Tasks[0].Status = StatusDone
	_ = p.Tasks[0].SetOutput("file_list", "parser.go, lex.go")
	_ = p.Tasks[0].SetOutput("summary", "parser in 42 lines")
	p.Tasks[1].Status = StatusActive

	ctx, err := p.RenderTaskContext("02")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Task 02: Validate",
		"Add validation rules.",
		"Inputs (resolved from prior tasks)",
		"**task 01 · file_list:**",
		"parser.go, lex.go",
		"Outputs to produce",
		"- `rules`",
		"- `summary`",
		"set_task_output",
		"complete_task",
		"get_task_output",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("RenderTaskContext missing %q\n---\n%s", want, ctx)
		}
	}
}

func TestRenderTaskContextUnknown(t *testing.T) {
	p, _ := NewFromArgs(samplePlanArgs())
	if _, err := p.RenderTaskContext("99"); err == nil {
		t.Error("expected error for unknown task number")
	}
}

func TestRenderTaskContextMissingInputValue(t *testing.T) {
	p, _ := NewFromArgs(samplePlanArgs())
	// Task 02 depends on 01.file_list but 01 never recorded it.
	p.Tasks[1].Status = StatusActive
	ctx, err := p.RenderTaskContext("02")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctx, "missing") {
		t.Errorf("missing-input marker not rendered:\n%s", ctx)
	}
}
