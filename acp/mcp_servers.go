package acp

import (
	"fmt"
	"sort"
	"strings"
)

// MCPServer is one MCP server an ACP agent connects to for a session. Set
// exactly one transport: URL (streamable HTTP, with optional Headers such as
// an Authorization token) or Command (stdio, with Args and Env).
type MCPServer struct {
	// Name is the server's name as the agent sees it. Required and unique.
	Name string

	// URL is the HTTP endpoint. Set this XOR Command.
	URL string
	// Headers are sent with every HTTP request to URL. HTTP only. Values
	// may be credentials: they go to the agent and nowhere else, and are
	// never put in a diagnostic.
	Headers map[string]string

	// Command is the stdio server executable. Set this XOR URL.
	Command string
	// Args is the stdio server argv, excluding Command.
	Args []string
	// Env is the stdio server environment.
	Env map[string]string
}

// SessionMCPServers renders servers in ACP's session/new and session/load
// "mcpServers" form (the shapes the ACP SDK's schema requires):
//
//	http:  {"type": "http", "name", "url", "headers": [{"name","value"}]}
//	stdio: {"name", "command", "args": [...], "env": [{"name","value"}]}
//
// headers, args and env are always arrays, and header and env entries are
// sorted by name. HTTP servers are sent only when the agent advertised
// agentCapabilities.mcpCapabilities.http at initialize; otherwise they are
// dropped and their names returned in skipped. A server with an empty or
// duplicate name, or without exactly one transport, is an error. The result
// is never nil, so "mcpServers" is always present on the wire.
//
// Known limit: an agent can accept the field and still not wire it. pi-acp
// advertises http: false and does not pass session mcpServers to pi at all,
// so Pi sessions get no MCP servers from here (stdio included).
func SessionMCPServers(servers []MCPServer, httpSupported bool) (wire []any, skipped []string, err error) {
	wire = []any{}
	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		switch {
		case s.Name == "":
			return nil, nil, fmt.Errorf("acp: MCP server with an empty name")
		case seen[s.Name]:
			return nil, nil, fmt.Errorf("acp: MCP server %q: duplicate name", s.Name)
		case (s.URL != "") == (s.Command != ""):
			return nil, nil, fmt.Errorf("acp: MCP server %q: set exactly one of URL or Command", s.Name)
		}
		seen[s.Name] = true
		if s.URL != "" {
			if !httpSupported {
				skipped = append(skipped, s.Name)
				continue
			}
			wire = append(wire, map[string]any{
				"type":    "http",
				"name":    s.Name,
				"url":     s.URL,
				"headers": nameValues(s.Headers),
			})
			continue
		}
		args := s.Args
		if args == nil {
			args = []string{}
		}
		wire = append(wire, map[string]any{
			"name":    s.Name,
			"command": s.Command,
			"args":    args,
			"env":     nameValues(s.Env),
		})
	}
	return wire, skipped, nil
}

// nameValues renders a map as ACP's [{"name","value"}] array, sorted by name.
func nameValues(m map[string]string) []any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{"name": k, "value": m[k]})
	}
	return out
}

// ReportSkippedMCPServers tells the host which HTTP MCP servers an agent
// without mcpCapabilities.http was not sent. Only names are reported.
func ReportSkippedMCPServers(onDiagnostic func(Diagnostic), skipped []string) {
	if onDiagnostic == nil || len(skipped) == 0 {
		return
	}
	onDiagnostic(NewDiagnostic(DiagnosticProtocol,
		"agent does not accept HTTP MCP servers (no mcpCapabilities.http); not sent: "+strings.Join(skipped, ", "), ""))
}
