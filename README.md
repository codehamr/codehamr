# codehamr

A minimal coding agent for the terminal. Built for local LLMs, also
runs on OpenAI compatible endpoints.

## Why minimal

A coding agent built for local LLMs has to make different decisions
than one built for frontier cloud models. Context is precious. Every
tool call has to earn its place. codehamr picks simplicity over
complexity, on purpose. The agent stays small so the context window
stays yours.

Three slash commands, one embedded system prompt, no router, no sub
agents, no skill system. That's it.

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

> Warning: codehamr is an AI system that runs model-generated commands with
> full shell and filesystem access. AI systems make mistakes. Run it
> inside a devcontainer or VM.

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
        context_size: 64000
    openai:
        llm: gpt-5.5
        url: https://api.openai.com
        key: sk-...
        context_size: 128000
    hamrpass:
        llm: hamrpass
        url: https://codehamr.com
        key: hp_...
        context_size: 512000
```

`/models` lists profiles, `/models <name>` switches.

## Keyboard

* `/` or `Tab` on an empty prompt opens the slash command popover
* `Tab` / `Shift+Tab` cycle, `Enter` accepts, `Esc` closes
* `↑` / `↓` walk through prior submissions, `Alt+Enter` inserts a newline
* `Ctrl+L` clears the prompt, `Ctrl+C` cancels or quits, `Ctrl+D` quits on empty

## Other tools in this space

If you want plugins, sub agents, or polished web clients,
[opencode](https://github.com/sst/opencode) and
[pi-coding-agent](https://github.com/badlogic/pi-mono) are excellent.
Claude Code and Codex are the polished commercial options.

## License

[MIT](LICENSE). Do whatever you want with it.

If codehamr is useful to you and you want to support development,
grab a [HamrPass](https://codehamr.com).