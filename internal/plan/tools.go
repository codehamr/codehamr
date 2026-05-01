package plan

// Plan-mode tools. These are the agent's only interface to plan state —
// the orchestrator intercepts calls by name and mutates the in-memory
// Plan directly. submit_plan creates the plan; set_task_output records a
// declared output on the active task; complete_task signals the task is
// done and lets the executor promote the next one; get_task_output is a
// fallback that retrieves a prior task's artifact when the input contract
// didn't anticipate a need.
//
// The schemas are returned as map[string]any so they can flow through the
// existing llm.Tool wrapping path (see tui.buildTools) exactly like the
// bash schema — no coupling from plan → llm.

const (
	ToolSubmitPlan    = "submit_plan"
	ToolSetTaskOutput = "set_task_output"
	ToolCompleteTask  = "complete_task"
	ToolGetTaskOutput = "get_task_output"
)

// IsPlanTool reports whether a tool-call name is one the orchestrator
// handles locally rather than dispatching to bash / MCP.
func IsPlanTool(name string) bool {
	switch name {
	case ToolSubmitPlan, ToolSetTaskOutput, ToolCompleteTask, ToolGetTaskOutput:
		return true
	}
	return false
}

// SubmitPlanSchema is the tool the agent calls in planning mode to hand
// the orchestrator a full typed plan. It's the ONLY tool exposed while
// planning — forces the model to decompose and declare I/O, not to start
// poking at bash prematurely.
func SubmitPlanSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        ToolSubmitPlan,
			"description": "Submit the full typed plan for the user's problem. Decompose into ≤12 flat tasks. Each task declares its inputs (backward refs to prior tasks' outputs) and outputs (structured data it produces). The executor runs tasks in order with fresh context, wiring declared inputs from recorded outputs. Call exactly once per planning turn.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"problem": map[string]any{
						"type":        "string",
						"description": "1-2 sentence statement of what the user wants solved.",
					},
					"intro": map[string]any{
						"type":        "string",
						"description": "Optional 1-3 sentences describing the macro strategy. Omit if the task list is self-evident.",
					},
					"tasks": map[string]any{
						"type":        "array",
						"description": "Flat, ordered task list. No nesting, no sub-plans.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"number": map[string]any{
									"type":        "string",
									"description": "Zero-padded two-digit number: '01', '02', …, '99'.",
								},
								"title": map[string]any{
									"type":        "string",
									"description": "Short imperative title — what this task delivers.",
								},
								"details": map[string]any{
									"type":        "string",
									"description": "2-6 sentence brief the agent will see in its fresh context: scope, approach, acceptance criteria. No stepwise instructions — the agent chooses steps.",
								},
								"inputs": map[string]any{
									"type":        "array",
									"description": "Outputs of earlier tasks that this task needs. Backward-only references, no cycles, no self-refs.",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"task":   map[string]any{"type": "string", "description": "Number of the earlier task, e.g. '02'."},
											"output": map[string]any{"type": "string", "description": "Output name declared on that task."},
										},
										"required": []string{"task", "output"},
									},
								},
								"outputs": map[string]any{
									"type":        "array",
									"description": "Structured data this task must produce. Every task declares at least a 'summary' output (terse handoff for later tasks).",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"name":        map[string]any{"type": "string", "description": "Short snake_case identifier: 'file_list', 'api_names', 'summary'."},
											"description": map[string]any{"type": "string", "description": "What this output carries and how later tasks will use it."},
										},
										"required": []string{"name", "description"},
									},
								},
							},
							"required": []string{"number", "title", "details", "outputs"},
						},
					},
				},
				"required": []string{"problem", "tasks"},
			},
		},
	}
}

// SetTaskOutputSchema is how the agent records one declared output value
// on the currently active task. Call once per declared output before
// complete_task — the orchestrator rejects complete_task until every
// declared output has a recorded value.
func SetTaskOutputSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        ToolSetTaskOutput,
			"description": "Record one of the active task's declared outputs. Name must match an output you declared in submit_plan. Value is plain text; serialise structured data as JSON or a newline-separated list.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":  map[string]any{"type": "string", "description": "Declared output name, e.g. 'summary'."},
					"value": map[string]any{"type": "string", "description": "The output value."},
				},
				"required": []string{"name", "value"},
			},
		},
	}
}

// CompleteTaskSchema is the agent's "this task is done" signal. The
// orchestrator refuses until every declared output has been set, then
// flips the task to done and promotes the next one.
func CompleteTaskSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        ToolCompleteTask,
			"description": "Signal the active task is complete. Every declared output must have been recorded via set_task_output first. After this call returns OK, end your turn — the executor advances to the next task.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// GetTaskOutputSchema is the fallback when an input wasn't wired upfront
// and the active task genuinely needs a prior artifact. The orchestrator
// records the call as a planning miss — use sparingly.
func GetTaskOutputSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        ToolGetTaskOutput,
			"description": "Fallback: fetch a prior task's recorded output when it wasn't declared as a current-task input. Logged as a planning miss; prefer declaring inputs upfront in submit_plan.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task":   map[string]any{"type": "string", "description": "Earlier task number."},
					"output": map[string]any{"type": "string", "description": "Output name on that task."},
				},
				"required": []string{"task", "output"},
			},
		},
	}
}

// PlanningTools is the plan-only tool set the orchestrator adds during
// plan creation: just submit_plan. The TUI still bundles bash + MCP into
// the chat payload so the model can investigate before producing the
// plan; the system prompt directs the agent to call submit_plan as the
// only plan-related action in this phase.
func PlanningTools() []map[string]any {
	return []map[string]any{SubmitPlanSchema()}
}

// ExecutionTools is the task-execution tool set that gets bundled with
// bash + MCP tools in the TUI's buildTools().
func ExecutionTools() []map[string]any {
	return []map[string]any{
		SetTaskOutputSchema(),
		CompleteTaskSchema(),
		GetTaskOutputSchema(),
	}
}
