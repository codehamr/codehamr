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

The agent runs one plain loop: it calls tools until the work is done,
then replies. A turn ends when it stops calling tools and hands control
back to you — no special end-of-turn ceremony. The agent works with
`bash`, `read_file`, `write_file`, and `edit_file`, investigating your
project directly rather than guessing, and verifies its own work (running
the tests, compiling, loading the page) as a habit the system prompt
instils — not a gate that blocks progress.

## Install

Linux, MacOS:

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

On first run codehamr seeds `.codehamr/config.yaml` with a `local`
(Ollama) profile and a `hamrpass` profile. The system prompt is embedded
in the binary, not on disk. Project specific rules go straight into the
chat: tell the agent what matters, the conversation carries it.

Any OpenAI-compatible endpoint works too — the example below adds an
`openai` profile:

```yaml
# codehamr configuration
#
# Running codehamr in a devcontainer / WSL2 with Ollama on the host:
# swap 'http://localhost:11434' with 'http://host.docker.internal:11434' below.

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

Local LLMs finally caught up, and we love it. For the best experience we recommend the **~30B class** like **qwen3.6:27b** on **32 GB+ unified RAM / VRAM**, fully local and a real alternative to expensive cloud subscriptions.

Info for Ollama users: Ollama Desktop may silently cut context to 4k. Open settings and lift the **Context length** slider to **64k+**, depending on your RAM / VRAM.

## Compare

| Tool | Pick if |
|---|---|
| **Frontier** | you want commercial heavyweight polish from Claude Code or Codex and accept the subscription cost and session timeouts |
| **[opencode](https://github.com/anomalyco/opencode)** | you want a great, loaded Swiss army knife and embrace plugin complexity |
| **[pi-agent](https://github.com/badlogic/pi-mono)** | you want something lighter than opencode and accept configuring your own extensions, skills, and themes |
| **codehamr** | you want the lightest take on simplicity over complexity and accept no plugins, skills, or sub-agents |

## HamrPass

We love local LLMs and always will. Codehamr is built fully open
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