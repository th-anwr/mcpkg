# Install format for agents

Every verified tool has three machine-readable views, all generated from the same data by code. The LLM never writes commands.

| URL | Format | For |
|---|---|---|
| `/tool/<slug>/install.md` | Markdown | agents that read docs (LLMs) |
| `/api/tools/<slug>/manifest.json` | JSON | programs, the finder MCP server |
| `/v0.1/servers` | MCP Registry OpenAPI shape + `_meta["<ns>/trust"]` | MCP clients that read registries |

## manifest.json

```json
{
  "$schema": "https://mcpkg.dev/schemas/install-manifest/v1.json",
  "name": "io.github.modelcontextprotocol/time",
  "slug": "io.github.modelcontextprotocol.time",
  "title": "time",
  "summary": "Time and timezone conversion",
  "version": "2026.10.19",
  "serverKey": "time",
  "pageUrl": "https://mcpkg.dev/tool/io.github.modelcontextprotocol.time",
  "installMdUrl": "https://mcpkg.dev/tool/io.github.modelcontextprotocol.time/install.md",
  "trust": { "score": 96, "grade": "A", "smokeStatus": "works", "status": "active", "verifiedAt": "2026-10-26T01:23:45Z" },
  "methods": [
    {
      "id": "pypi", "label": "PyPI (uvx)", "transport": "stdio",
      "command": "uvx", "args": ["mcp-server-time==2026.10.19"],
      "requires": ["uv (https://docs.astral.sh/uv/)"],
      "clients": [
        { "client": "claude-code", "kind": "cli", "content": "claude mcp add time -- uvx mcp-server-time==2026.10.19" },
        { "client": "claude-desktop", "kind": "json", "location": "claude_desktop_config.json", "content": "{ \"mcpServers\": { ... } }" },
        { "client": "cursor", "kind": "json", "location": "~/.cursor/mcp.json", "content": "..." },
        { "client": "vscode", "kind": "json", "location": ".vscode/mcp.json", "content": "..." },
        { "client": "codex", "kind": "toml", "location": "~/.codex/config.toml", "content": "[mcp_servers.time] ..." }
      ]
    }
  ],
  "env": [
    { "name": "API_KEY", "required": true, "secret": true, "description": "…", "help": "Where to get it…", "placeholder": "<API_KEY>" }
  ],
  "expectedTools": ["get_current_time", "convert_time"],
  "agentSteps": ["Ask the user for: … Never invent …", "Pick the user's MCP client …", "Restart …", "Verify: …"],
  "warnings": [],
  "generatedAt": "2026-10-26T01:23:45Z"
}
```

## Rules

- **Methods are ordered.** The method the smoke test verified comes first.
- **Exact versions.** Commands pin the version that was tested (`pkg@1.2.3`, `pkg==1.2.3`).
- **Exact executables.** When a package's bin or entry point differs from its name, the generator uses `npx --package=<pkg> <bin>` or `uvx --from <pkg> <entry>`.
- **Secrets.** Only required variables are written into configs, as `<NAME>` placeholders. VS Code configs use `${input:…}` prompts, so secrets aren't stored in the file.
- **Remote servers.**
  - Claude Code: `claude mcp add --transport http|sse`.
  - Cursor and VS Code: `url`.
  - Claude Desktop and Codex: the `mcp-remote` bridge.
- **expectedTools** comes from the live `tools/list`. Agents use it to verify the install worked.

## Versioning

Add fields freely. Removing or renaming a field requires `/v2/` in `$schema`.
