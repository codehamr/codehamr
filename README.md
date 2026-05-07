# codehamr

A minimal coding agent for the terminal. Built for local LLMs, also
runs on OpenAI compatible endpoints.

## Simplicity

A coding agent built for local LLMs has to make different decisions
than one built for frontier cloud models. Context is precious. Every
tool call has to earn its place. codehamr picks simplicity over
complexity, on purpose. The agent stays small so the context window
stays yours.

Three slash commands, one embedded system prompt, no router, no
sub-agents, no skill system, no MCP. That's it.

The agent runs in one deterministic loop, internally called *GYSD*
(Get Your Shit Done), where every turn ends with one of three tools:
`verify` (run a check), `done` (claim completion, must quote a passing
verify as proof), or `ask` (yield back to you). No hallucinated success.

## Install

Linux, macOS:

```bash
curl -fsSL https://codehamr.com/install.sh | bash
```

Windows:

```cmd
curl -fsSL https://codehamr.com/install.cmd -o install.cmd && install.cmd
```

Then run `codehamr` in your project.

> **Warning:** AI systems like codehamr run model-generated shell commands with full filesystem access. Best run inside safe sandboxes like devcontainers or isolated VMs.

## Config

On first run codehamr creates `.codehamr/config.yaml` for your
profiles. The system prompt is embedded in the binary, not on disk.
Project specific rules go straight into the chat: tell the agent
what matters, the conversation carries it.

```yaml
# .codehamr/config.yaml
active: local
models:
    local:
        llm: qwen3.6:27b
        url: http://localhost:11434
        key: ""
        context_size: 131072
    openai:
        llm: gpt-5.5
        url: https://api.openai.com
        key: sk-...
        context_size: 131072
    hamrpass:
        llm: hamrpass
        url: https://codehamr.com
        key: hp_...
```

`/models` lists profiles, `/models <name>` switches.

## Hardware

codehamr is tuned for the **~27B LLM class**, like **qwen3.6:27b** at 128k context, which needs **32 GB+ unified memory or VRAM**. Ollama desktop users must lift "Context length" in the app settings themselves, otherwise it caps at 4k silently.

Bare minimum fallback: the **~9B LLM class**, like **qwen3.5:9b** at 64k on **12 GB+ unified memory or VRAM**. Quality drops noticeably with weaker reasoning, more verify retries, and more loop slips. If neither tier fits your machine, give the optional HamrPass a try, or use your own OpenAI API key.

## Compare

| Tool | Pick if |
|---|---|
| **Frontier** | you want commercial heavyweight polish from Claude Code or Codex and accept the subscription cost and session timeouts |
| **[opencode](https://github.com/anomalyco/opencode)** | you want a great, loaded Swiss army knife and embrace plugin complexity |
| **[pi-coding-agent](https://github.com/badlogic/pi-mono)** | you want something lighter than opencode and accept configuring your own extensions, skills, and themes |
| **codehamr** | you want the lightest take on simplicity over complexity and accept no plugins, skills, or sub-agents |

## HamrPass

We love local LLMs and always will. codehamr is built fully open
source with an MIT license and always will be. Connect to your
local Ollama models, or bring your own key with OpenRouter, OpenAI,
whatever you like.

HamrPass is an optional alternative. It's there if you want to
support the project, or if you'd rather not spend your weekend
benchmarking the latest open weight model and tuning every
parameter. We do that work and ship it as one endpoint with sensible
defaults, so you can just hamr code and get your shit done.

There's a waitlist at [codehamr.com](https://codehamr.com). HamrPass only gets built if real demand shows up there. Otherwise it doesn't. Local-first stays the focus.

## License

[MIT](LICENSE). Do whatever you want with it. Star it if it earned one.