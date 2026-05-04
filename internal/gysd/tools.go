package gysd

// Tool schemas — the OpenAI-compatible map[string]any shape that the TUI
// wraps into llm.Tool via schemaToTool. Three tools, one description each,
// no nested objects beyond what the LLM needs to fill out. Brevity on the
// schema side helps mid-size local LLMs (Qwen-class 27b, Llama 3.1 70b)
// keep tool-call arguments well-formed.

// VerifySchema runs a shell command and stores its output as potential
// evidence. The description doubles as a one-shot reminder of the loop
// rule — local LLMs often re-read tool descriptions when deciding which
// tool to call next.
func VerifySchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        ToolVerify,
			"description": "Run a shell command and capture its result as a verification checkpoint. Output is shown live and stored as potential evidence for `done`. Use after meaningful work to prove the code does what the goal demands. Be specific to what you just changed (e.g. `pytest tests/test_X.py`, `go test ./pkg`, `grep -q PATTERN file`). NEVER trivial like `true`, `:`, or `echo done`. For ops longer than 10 min: spawn via bash (`nohup cmd > /tmp/out.log 2>&1 &`), poll the pid, then verify with a fast `grep`/`tail` on the log file.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "Shell command, executed via /bin/sh -c. Specific to the change you just made.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Optional per-call timeout in seconds. Default 60, hard cap 600 (10 min). Raise for slow builds; for genuinely longer ops use the background-spawn pattern.",
						"minimum":     1,
						"maximum":     600,
					},
				},
				"required": []string{"command"},
			},
		},
	}
}

// DoneSchema signals the goal is reached. evidence must be a >=20-char
// verbatim substring from a green verify in this loop — the orchestrator
// checks this against its own VerifyLog. Models cannot fake completion.
func DoneSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        ToolDone,
			"description": "Signal the goal is reached. Provide a 1-2 sentence summary and quote a verbatim substring (>=20 chars) from a green verify in this loop as evidence. Rejected if no green verify matches the evidence string.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{
						"type":        "string",
						"description": "1-2 sentence description of what was done, for the user.",
					},
					"evidence": map[string]any{
						"type":        "string",
						"description": "Substring (>=20 chars) copied verbatim from a green verify's output in this loop. The orchestrator validates the match.",
					},
				},
				"required": []string{"summary", "evidence"},
			},
		},
	}
}

// AskSchema yields to the user with a concrete question. The honest
// end-state for creative-open work where no automatic check applies, or
// when the model is genuinely stuck and needs a decision.
func AskSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        ToolAsk,
			"description": "Yield to the user with a concrete question. Use when stuck, when the goal is creative-open without an automatic check, or when a non-trivial decision is needed.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{
						"type":        "string",
						"description": "Concrete question for the user, >=8 chars after trim.",
					},
				},
				"required": []string{"question"},
			},
		},
	}
}

// LoopTools is the full GYSD tool set. Always exposed alongside bash and
// write_file — no phase-gating, no triage. One mode, three loop tools.
func LoopTools() []map[string]any {
	return []map[string]any{
		VerifySchema(),
		DoneSchema(),
		AskSchema(),
	}
}
