// Package plan owns the typed, in-memory plan: tasks with declared inputs
// (refs to prior tasks' outputs) and declared outputs (structured data the
// task must produce). The executor drives tasks forward deterministically;
// the agent never touches state — it calls tools (submit_plan,
// set_task_output, complete_task, get_task_output) and the orchestrator
// mutates the plan.
//
// No disk persistence. No markdown parsing. Plans live for one session; if
// the process crashes mid-plan, the plan is lost — same as chat history.
// *hamr stays hamr.*
package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	chmctx "github.com/codehamr/codehamr/internal/ctx"
)

// Status is the lifecycle state of a single task.
type Status int

const (
	StatusOpen Status = iota
	StatusActive
	StatusDone
)

func (s Status) String() string {
	switch s {
	case StatusOpen:
		return "open"
	case StatusActive:
		return "active"
	case StatusDone:
		return "done"
	}
	return "?"
}

// InputRef points to one named output of an earlier task. Backward refs
// only — forward refs and self-refs are rejected at Validate time.
type InputRef struct {
	Task   string `json:"task"`
	Output string `json:"output"`
}

// OutputSpec declares one structured value a task must produce. The agent
// reports each declared output via set_task_output before calling
// complete_task; the orchestrator refuses to advance until every declared
// output has a recorded value.
type OutputSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Task is one atom of a plan. Number is zero-padded (01..99), Title is a
// short imperative, Details is the full brief the agent sees in its fresh
// context. Inputs declare the data flow from prior tasks; Outputs declare
// the data this task contributes to later tasks. Status and Results are
// runtime state, never serialized in submit_plan.
type Task struct {
	Number  string       `json:"number"`
	Title   string       `json:"title"`
	Details string       `json:"details"`
	Inputs  []InputRef   `json:"inputs,omitempty"`
	Outputs []OutputSpec `json:"outputs"`

	Status  Status            `json:"-"`
	Results map[string]string `json:"-"`
}

// Plan is the whole macro-plan: a problem statement, an optional intro
// describing the strategy, a flat list of tasks (no nesting, no recursion),
// and a FinalSummary filled at wrap-up time.
type Plan struct {
	Problem      string `json:"problem"`
	Intro        string `json:"intro,omitempty"`
	Tasks        []Task `json:"tasks"`
	FinalSummary string `json:"-"`
}

// numberRE enforces two-digit zero-padded task numbers. Keeps the chat
// renderer's "Task 03" formatting uniform and catches agents who try to
// submit "3" or "task-3".
var numberRE = regexp.MustCompile(`^\d{2}$`)

// NewFromArgs hydrates a Plan from the raw map[string]any the LLM client
// hands back for a submit_plan tool call. Runs the same Validate that
// guards in-memory mutations — invalid plans never reach the executor.
func NewFromArgs(args map[string]any) (*Plan, error) {
	buf, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("re-marshal args: %w", err)
	}
	var p Plan
	dec := json.NewDecoder(bytes.NewReader(buf))
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	for i := range p.Tasks {
		p.Tasks[i].Results = map[string]string{}
	}
	return &p, nil
}

// Validate enforces the structural invariants the executor relies on:
// non-empty problem, ≥1 task, zero-padded two-digit unique numbers, at
// least one declared output per task, and backward-only input references
// whose targets actually exist. An invalid plan is rejected loud rather
// than silently mutated — the agent must resubmit.
func (p *Plan) Validate() error {
	if strings.TrimSpace(p.Problem) == "" {
		return fmt.Errorf("plan: problem is empty")
	}
	if len(p.Tasks) == 0 {
		return fmt.Errorf("plan: no tasks")
	}
	seen := map[string]int{}
	for i, t := range p.Tasks {
		if !numberRE.MatchString(t.Number) {
			return fmt.Errorf("task %q: number must be two zero-padded digits", t.Number)
		}
		if prev, ok := seen[t.Number]; ok {
			return fmt.Errorf("task %s: duplicate number (first seen at index %d)", t.Number, prev)
		}
		seen[t.Number] = i
		if strings.TrimSpace(t.Title) == "" {
			return fmt.Errorf("task %s: title is empty", t.Number)
		}
		if strings.TrimSpace(t.Details) == "" {
			return fmt.Errorf("task %s: details are empty", t.Number)
		}
		if len(t.Outputs) == 0 {
			return fmt.Errorf("task %s: must declare at least one output (conventionally 'summary')", t.Number)
		}
		outNames := map[string]bool{}
		for _, o := range t.Outputs {
			if strings.TrimSpace(o.Name) == "" {
				return fmt.Errorf("task %s: output with empty name", t.Number)
			}
			if outNames[o.Name] {
				return fmt.Errorf("task %s: duplicate output name %q", t.Number, o.Name)
			}
			outNames[o.Name] = true
		}
	}
	// Second pass: every InputRef must point to a task that appears earlier
	// and declares the named output. Walking in index order makes this O(n²)
	// at most, which is fine for ≤ ~20 tasks.
	for i, t := range p.Tasks {
		for _, in := range t.Inputs {
			target, ok := seen[in.Task]
			if !ok {
				return fmt.Errorf("task %s: input refs unknown task %q", t.Number, in.Task)
			}
			if target >= i {
				return fmt.Errorf("task %s: input refs task %s which is not earlier (cycle or self-ref)", t.Number, in.Task)
			}
			var found bool
			for _, o := range p.Tasks[target].Outputs {
				if o.Name == in.Output {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("task %s: input refs task %s output %q which was not declared", t.Number, in.Task, in.Output)
			}
		}
	}
	return nil
}

// Find returns the task with the given number, or nil if none match.
func (p *Plan) Find(number string) *Task {
	for i := range p.Tasks {
		if p.Tasks[i].Number == number {
			return &p.Tasks[i]
		}
	}
	return nil
}

// firstWithStatus returns the first task in declaration order matching
// the given status, or nil.
func (p *Plan) firstWithStatus(s Status) *Task {
	for i := range p.Tasks {
		if p.Tasks[i].Status == s {
			return &p.Tasks[i]
		}
	}
	return nil
}

// Active returns the task with StatusActive, or nil. The executor
// guarantees at most one active task at any time.
func (p *Plan) Active() *Task { return p.firstWithStatus(StatusActive) }

// FirstOpen returns the first StatusOpen task in declaration order, or nil.
func (p *Plan) FirstOpen() *Task { return p.firstWithStatus(StatusOpen) }

// MarkActive promotes an Open task to Active. Errors on unknown number or
// wrong source state.
func (p *Plan) MarkActive(number string) error {
	return p.transition(number, StatusOpen, StatusActive)
}

// MarkDone promotes an Active task to Done. Called by the executor once
// the agent's complete_task signal arrives and every declared output has
// a recorded value.
func (p *Plan) MarkDone(number string) error {
	return p.transition(number, StatusActive, StatusDone)
}

// transition mutates a task's Status from -> to and names the target
// state in the error message when the precondition fails. Unknown task
// numbers and wrong source states both surface as errors the caller can
// bubble straight to the agent.
func (p *Plan) transition(number string, from, to Status) error {
	t := p.Find(number)
	if t == nil {
		return fmt.Errorf("task %s not found", number)
	}
	if t.Status != from {
		return fmt.Errorf("task %s: %s, cannot mark %s", number, t.Status, to)
	}
	t.Status = to
	return nil
}

// AllDone reports whether every task is StatusDone. Empty plans never are.
func (p *Plan) AllDone() bool {
	if len(p.Tasks) == 0 {
		return false
	}
	for i := range p.Tasks {
		if p.Tasks[i].Status != StatusDone {
			return false
		}
	}
	return true
}

// Total returns the task count. Handy for "03/07" displays.
func (p *Plan) Total() int { return len(p.Tasks) }

// GetOutput looks up a recorded output value from a prior task. Used both
// for input resolution at task start and for the get_task_output fallback
// tool when the agent needs something that wasn't wired upfront.
func (p *Plan) GetOutput(taskNumber, name string) (string, bool) {
	t := p.Find(taskNumber)
	if t == nil || t.Results == nil {
		return "", false
	}
	v, ok := t.Results[name]
	return v, ok
}

// SetOutput records a value for one declared output on this task. Rejects
// names that weren't declared — the plan's output contract is binding.
func (t *Task) SetOutput(name, value string) error {
	declared := false
	for _, o := range t.Outputs {
		if o.Name == name {
			declared = true
			break
		}
	}
	if !declared {
		return fmt.Errorf("task %s: output %q was not declared", t.Number, name)
	}
	if t.Results == nil {
		t.Results = map[string]string{}
	}
	t.Results[name] = value
	return nil
}

// OutputsComplete returns nil if every declared output has a recorded value,
// otherwise a human-readable error naming the missing output. Called before
// MarkDone to refuse premature task closure.
func (t *Task) OutputsComplete() error {
	for _, o := range t.Outputs {
		if _, ok := t.Results[o.Name]; !ok {
			return fmt.Errorf("task %s: output %q not set", t.Number, o.Name)
		}
	}
	return nil
}

// RenderOverview is the compact progress view shown at the top of every
// task's fresh context — problem, intro, and a status-marked task list.
// Tight so the agent isn't reading prose when it should be working.
func (p *Plan) RenderOverview() string {
	var b strings.Builder
	b.WriteString("# Plan: ")
	b.WriteString(p.Problem)
	b.WriteString("\n")
	if strings.TrimSpace(p.Intro) != "" {
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(p.Intro))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	for _, t := range p.Tasks {
		fmt.Fprintf(&b, "- [%s] %s %s\n", boxFromStatus(t.Status), t.Number, t.Title)
	}
	return b.String()
}

// RenderTaskContext builds the single-message user prompt handed to the
// agent when a task turn starts: overview + active task's full brief +
// resolved inputs + outputs contract + workflow instructions. This IS the
// context — no other state carries over from prior tasks except declared,
// resolved input values.
func (p *Plan) RenderTaskContext(number string) (string, error) {
	task := p.Find(number)
	if task == nil {
		return "", fmt.Errorf("task %s not found", number)
	}
	var b strings.Builder
	b.WriteString(p.RenderOverview())
	b.WriteString("\n## Task ")
	b.WriteString(task.Number)
	b.WriteString(": ")
	b.WriteString(task.Title)
	b.WriteString("\n\n")
	b.WriteString(strings.TrimSpace(task.Details))
	b.WriteString("\n")

	if len(task.Inputs) > 0 {
		b.WriteString("\n### Inputs (resolved from prior tasks)\n")
		for _, in := range task.Inputs {
			fmt.Fprintf(&b, "\n**task %s · %s:**\n", in.Task, in.Output)
			val, ok := p.GetOutput(in.Task, in.Output)
			if !ok {
				b.WriteString("(missing — upstream task did not record this output; call get_task_output if you need detail)\n")
				continue
			}
			// Guard against agents that stored oversized outputs: a single
			// input running into tens of thousands of tokens would dominate
			// the fresh task context and push real work off the screen.
			// Truncate mirrors the bash-output contract so the agent can
			// call get_task_output for detail if it truly needs the rest.
			val = chmctx.Truncate(val)
			b.WriteString(val)
			if !strings.HasSuffix(val, "\n") {
				b.WriteString("\n")
			}
		}
	}

	b.WriteString("\n### Outputs to produce\n\n")
	for _, out := range task.Outputs {
		fmt.Fprintf(&b, "- `%s` — %s\n", out.Name, out.Description)
	}

	b.WriteString("\n### Workflow\n\n")
	b.WriteString("1. Do the work via tools (bash, MCP, etc.).\n")
	b.WriteString("2. For each declared output above, call `set_task_output(name=..., value=...)`.\n")
	b.WriteString("3. When all outputs are set, call `complete_task()` — the executor promotes the next task.\n")
	b.WriteString("4. If you genuinely need a prior task's artifact that wasn't declared as input, call `get_task_output(task=..., output=...)`.\n")
	b.WriteString("5. Do not call `submit_plan` — tasks never spawn sub-plans.\n")

	return b.String(), nil
}

func boxFromStatus(s Status) string {
	switch s {
	case StatusOpen:
		return " "
	case StatusActive:
		return "~"
	case StatusDone:
		return "x"
	}
	return "?"
}
