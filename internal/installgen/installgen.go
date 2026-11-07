// Package installgen generates exact, copy-pasteable install instructions for
// every popular MCP client from verified metadata. It is template-driven and
// deterministic: the LLM never writes commands.
package installgen

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mcpkg/mcpkg/internal/identity"
	"github.com/mcpkg/mcpkg/internal/model"
)

// SchemaURL identifies the manifest format (docs/INSTALL_FORMAT.md).
const SchemaURL = "https://mcpkg.dev/schemas/install-manifest/v1.json"

// Build creates the install manifest for a tool.
func Build(t *model.Tool, siteURL string, now time.Time) *model.Manifest {
	key := identity.ServerKey(t.Name)
	m := &model.Manifest{
		Schema:    SchemaURL,
		Name:      t.Name,
		Slug:      t.Slug,
		Title:     t.DisplayName(),
		Summary:   t.Summary,
		Version:   t.Version,
		ServerKey: key,
		PageURL:   siteURL + "/tool/" + t.Slug,
		InstallMD: siteURL + "/tool/" + t.Slug + "/install.md",
		Generated: now,
	}
	if t.Score != nil {
		m.Trust = model.ManifestTrust{Score: t.Score.Total, Grade: t.Score.Grade}
	}
	m.Trust.SmokeStatus, m.Trust.Status, m.Trust.VerifiedAt = t.SmokeStatus, t.Status, t.VerifiedAt
	m.Env = envList(t)
	// Generated configs carry required variables only; optional ones are documented in Env.
	envVals := map[string]string{}
	for _, e := range m.Env {
		if e.Required {
			envVals[e.Name] = e.Placeholder
		}
	}
	for i, p := range t.Packages {
		if meth, ok := packageMethod(t, i, p, key, envVals); ok {
			m.Methods = append(m.Methods, meth)
		}
	}
	for _, r := range t.Remotes {
		m.Methods = append(m.Methods, remoteMethod(r, key))
	}
	// Prefer the method the smoke test verified.
	if t.Smoke != nil && t.Smoke.Status == "works" {
		sort.SliceStable(m.Methods, func(i, j int) bool { return m.Methods[i].ID == t.Smoke.Method && m.Methods[j].ID != t.Smoke.Method })
	}
	if t.Smoke != nil {
		for _, tool := range t.Smoke.Tools {
			m.Expected = append(m.Expected, tool.Name)
		}
	}
	m.AgentSteps = agentSteps(m)
	m.Warnings = warnings(t)
	return m
}

func envList(t *model.Tool) []model.ManifestEnv {
	seen := map[string]bool{}
	var out []model.ManifestEnv
	for _, p := range t.Packages {
		for _, v := range p.EnvironmentVariables {
			if v.Name == "" || seen[v.Name] {
				continue
			}
			seen[v.Name] = true
			e := model.ManifestEnv{Name: v.Name, Required: v.IsRequired, Secret: v.IsSecret, Description: v.Description,
				Default: v.Default, Placeholder: "<" + v.Name + ">"}
			if v.Default != "" && !v.IsSecret {
				e.Placeholder = v.Default
			}
			if t.Enrich != nil && t.Enrich.EnvHelp[v.Name] != "" {
				e.Help = t.Enrich.EnvHelp[v.Name]
			}
			out = append(out, e)
		}
	}
	return out
}

func packageMethod(t *model.Tool, idx int, p model.Package, key string, env map[string]string) (model.InstallMethod, bool) {
	var cmd string
	var args []string
	var requires []string
	ver := p.Version
	switch p.RegistryType {
	case "npm":
		cmd = "npx"
		spec := p.Identifier
		if ver != "" {
			spec += "@" + ver
		}
		args = []string{"-y", spec}
		short := p.Identifier[strings.LastIndex(p.Identifier, "/")+1:]
		if p.Bin != "" && p.Bin != short {
			args = []string{"-y", "--package=" + spec, p.Bin}
		}
		requires = []string{"Node.js 18+"}
	case "pypi":
		cmd = "uvx"
		spec := p.Identifier
		if ver != "" {
			spec += "==" + ver
		}
		if p.Bin != "" && identity.PyPIName(p.Bin) != identity.PyPIName(p.Identifier) {
			args = []string{"--from", spec, p.Bin}
		} else {
			args = []string{spec}
		}
		requires = []string{"uv (https://docs.astral.sh/uv/)"}
	case "oci":
		cmd = "docker"
		args = []string{"run", "-i", "--rm"}
		names := sortedKeys(env)
		for _, n := range names {
			args = append(args, "-e", n)
		}
		args = append(args, p.Identifier)
		requires = []string{"Docker"}
	default:
		return model.InstallMethod{}, false
	}
	if p.Transport.Type != "" && p.Transport.Type != "stdio" {
		return model.InstallMethod{}, false // local HTTP servers: documented in README only for now
	}
	args = append(args, packageArgs(p)...)
	meth := model.InstallMethod{ID: p.RegistryType, Label: labelFor(p.RegistryType), Transport: "stdio",
		Command: cmd, Args: args, Requires: requires}
	meth.Clients = stdioClients(key, cmd, args, env)
	return meth, true
}

func packageArgs(p model.Package) []string {
	var out []string
	for _, a := range p.PackageArguments {
		v := a.Value
		if v == "" {
			v = a.Default
		}
		if v == "" && a.IsRequired {
			hint := a.ValueHint
			if hint == "" {
				hint = a.Name
			}
			if hint == "" {
				hint = "value"
			}
			v = "<" + strings.TrimLeft(hint, "-") + ">"
		}
		if v == "" {
			continue
		}
		if a.Type == "named" && a.Name != "" {
			out = append(out, a.Name, v)
		} else {
			out = append(out, v)
		}
	}
	return out
}

func labelFor(kind string) string {
	return map[string]string{"npm": "npm (npx)", "pypi": "PyPI (uvx)", "oci": "Docker", "remote": "Remote (hosted)"}[kind]
}

func remoteMethod(r model.Remote, key string) model.InstallMethod {
	transport := r.Type
	if transport == "" {
		transport = "streamable-http"
	}
	meth := model.InstallMethod{ID: "remote", Label: labelFor("remote"), Transport: transport, URL: r.URL, Headers: r.Headers}
	cliTransport := "http"
	if transport == "sse" {
		cliTransport = "sse"
	}
	headers := map[string]string{}
	for _, h := range r.Headers {
		val := h.Value
		if val == "" || strings.Contains(val, "{") {
			val = "<" + h.Name + ">"
		}
		headers[h.Name] = val
	}
	var cc strings.Builder
	fmt.Fprintf(&cc, "claude mcp add --transport %s %s %s", cliTransport, key, r.URL)
	for _, n := range sortedKeys(headers) {
		fmt.Fprintf(&cc, " --header %s", shellQuote(n+": "+headers[n]))
	}
	cursor := map[string]any{"url": r.URL}
	vscode := map[string]any{"type": cliTransport, "url": r.URL}
	if len(headers) > 0 {
		cursor["headers"] = headers
		vscode["headers"] = headers
	}
	bridgeArgs := []string{"-y", "mcp-remote", r.URL}
	for _, n := range sortedKeys(headers) {
		bridgeArgs = append(bridgeArgs, "--header", n+": "+headers[n])
	}
	meth.Clients = []model.ClientConfig{
		{Client: "claude-code", Label: "Claude Code", Kind: "cli", Content: cc.String()},
		{Client: "claude-desktop", Label: "Claude Desktop", Kind: "json", Location: "Settings → Connectors → Add custom connector (or claude_desktop_config.json via mcp-remote)",
			Content: pretty(map[string]any{"mcpServers": map[string]any{key: map[string]any{"command": "npx", "args": bridgeArgs}}})},
		{Client: "cursor", Label: "Cursor", Kind: "json", Location: "~/.cursor/mcp.json", Content: pretty(map[string]any{"mcpServers": map[string]any{key: cursor}})},
		{Client: "vscode", Label: "VS Code", Kind: "json", Location: ".vscode/mcp.json", Content: pretty(map[string]any{"servers": map[string]any{key: vscode}})},
		{Client: "codex", Label: "Codex CLI", Kind: "toml", Location: "~/.codex/config.toml", Content: tomlServer(key, "npx", bridgeArgs, nil)},
	}
	return meth
}

func stdioClients(key, cmd string, args []string, env map[string]string) []model.ClientConfig {
	var cc strings.Builder
	fmt.Fprintf(&cc, "claude mcp add %s", key)
	for _, n := range sortedKeys(env) {
		fmt.Fprintf(&cc, " -e %s", shellQuote(n+"="+env[n]))
	}
	cc.WriteString(" -- " + cmd)
	for _, a := range args {
		cc.WriteString(" " + shellQuote(a))
	}
	server := map[string]any{"command": cmd, "args": args}
	if len(env) > 0 {
		server["env"] = env
	}
	// VS Code can prompt for secrets instead of storing them in the file.
	vsEnv := map[string]string{}
	var inputs []map[string]any
	for _, n := range sortedKeys(env) {
		id := strings.ToLower(strings.ReplaceAll(n, "_", "-"))
		vsEnv[n] = "${input:" + id + "}"
		inputs = append(inputs, map[string]any{"type": "promptString", "id": id, "description": n, "password": true})
	}
	vs := map[string]any{"type": "stdio", "command": cmd, "args": args}
	vsDoc := map[string]any{"servers": map[string]any{key: vs}}
	if len(env) > 0 {
		vs["env"] = vsEnv
		vsDoc["inputs"] = inputs
	}
	return []model.ClientConfig{
		{Client: "claude-code", Label: "Claude Code", Kind: "cli", Content: cc.String()},
		{Client: "claude-desktop", Label: "Claude Desktop", Kind: "json", Location: "claude_desktop_config.json",
			Content: pretty(map[string]any{"mcpServers": map[string]any{key: server}})},
		{Client: "cursor", Label: "Cursor", Kind: "json", Location: "~/.cursor/mcp.json", Content: pretty(map[string]any{"mcpServers": map[string]any{key: server}})},
		{Client: "vscode", Label: "VS Code", Kind: "json", Location: ".vscode/mcp.json", Content: pretty(vsDoc)},
		{Client: "codex", Label: "Codex CLI", Kind: "toml", Location: "~/.codex/config.toml", Content: tomlServer(key, cmd, args, env)},
	}
}

func agentSteps(m *model.Manifest) []string {
	var steps []string
	var need []string
	for _, e := range m.Env {
		if e.Required {
			need = append(need, e.Name)
		}
	}
	if len(need) > 0 {
		steps = append(steps, "Ask the user for: "+strings.Join(need, ", ")+". Never invent or guess these values; secrets go into the client config, not into chat logs.")
	}
	steps = append(steps, "Pick the user's MCP client and apply the matching config from the first install method (it is the one verified by the smoke test when available).")
	steps = append(steps, "Restart or reload the client so it starts the server.")
	if len(m.Expected) > 0 {
		shown := m.Expected
		if len(shown) > 8 {
			shown = append(shown[:8:8], "…")
		}
		steps = append(steps, "Verify: the server should expose these tools: "+strings.Join(shown, ", ")+".")
	}
	steps = append(steps, "If it fails, check that the required runtime is installed ("+requiresOf(m)+") and that the values above are set.")
	return steps
}

func requiresOf(m *model.Manifest) string {
	if len(m.Methods) == 0 || len(m.Methods[0].Requires) == 0 {
		return "none"
	}
	return strings.Join(m.Methods[0].Requires, ", ")
}

func warnings(t *model.Tool) []string {
	var w []string
	switch t.SmokeStatus {
	case "broken", "install_failed":
		w = append(w, "This server failed our last smoke test; it may not start.")
	case "skipped":
		if t.Smoke != nil && t.Smoke.SkipReason != "" {
			w = append(w, "Not smoke-tested: "+t.Smoke.SkipReason+".")
		}
	}
	if t.Status == "unmaintained" {
		w = append(w, "The repository is archived or inactive.")
	}
	if t.Status == "deprecated" {
		w = append(w, "Deprecated by its publisher in the MCP Registry.")
	}
	if t.Security != nil && len(t.Security.InstallScripts) > 0 {
		w = append(w, "Runs scripts at install time.")
	}
	if t.Security != nil && t.Security.NeedsReview {
		w = append(w, "Has high-severity static-analysis findings pending human review.")
	}
	return w
}

func tomlServer(key, cmd string, args []string, env map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[mcp_servers.%s]\ncommand = %s\nargs = [", tomlKey(key), tomlString(cmd))
	for i, a := range args {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tomlString(a))
	}
	b.WriteString("]\n")
	if len(env) > 0 {
		b.WriteString("env = { ")
		for i, n := range sortedKeys(env) {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s = %s", tomlString(n), tomlString(env[n]))
		}
		b.WriteString(" }\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func tomlKey(k string) string {
	for _, r := range k {
		if !(r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return tomlString(k)
		}
	}
	return k
}

func tomlString(s string) string {
	// JSON string escaping (without HTML escaping) is valid TOML basic-string escaping.
	return strings.TrimSuffix(encode(s, ""), "\n")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:@+,", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func pretty(v any) string { return strings.TrimSuffix(encode(v, "  "), "\n") }

// encode marshals without escaping <, > and & (placeholders like <API_KEY> stay readable).
func encode(v any, indent string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	_ = enc.Encode(v)
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
