# Harness manifests

One YAML file per harness. `mycel agent setup <harness> --agent <name>` reads
the manifest and writes the harness's own configuration so that the harness
connects to Mycelium through `mycel mcp` and, where supported, injects new
inbox messages at each turn through `mycel hook`.

Fields:

```yaml
name: claude-code            # manifest id, [a-z0-9-]
display_name: Claude Code
install: npm install -g @anthropic-ai/claude-code   # informational; `setup` does not run it
detect: claude --version     # command that succeeds when the harness is installed
mcp:                         # how to register the MCP server with this harness
  file: .mcp.json            # relative to the project directory
  entry:                     # JSON merged under "mcpServers"
    mycel:
      command: mycel
      args: ["mcp", "--agent", "{{agent}}", "--harness", "claude-code"]
hooks:                       # optional; harnesses without hooks omit this
  file: .claude/settings.json
  entries:                   # merged into the harness's hook config
    UserPromptSubmit:
      - command: mycel hook --agent {{agent}}
```

`{{agent}}` is replaced by the agent name. Phase 3 adds `spawn` (how to start
the harness headless with a task).
