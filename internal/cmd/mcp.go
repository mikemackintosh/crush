package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/shellconfig"
	"github.com/spf13/cobra"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Manage MCP servers",
	Long: `Add, remove and list Model Context Protocol servers.

Changes are written to the global Crush config so nothing has to be edited by
hand; pass --workspace to write to this project's .crush/crush.json instead.
"crush mcp add" takes the same flags as the "mcp add" line in a crushrc file.`,
	Example: `
# A local server over stdio
crush mcp add filesystem --command npx --args -y --args @modelcontextprotocol/server-filesystem --args ~/src

# A remote server with a bearer token
crush mcp add github --type http --url https://api.githubcopilot.com/mcp/ --header Authorization "Bearer $GH_PAT"

# A remote server behind an OAuth 2.1 identity provider (discovery + dynamic registration)
crush mcp add rocketbox --type http --url https://rocketbox.ai/mcp --oauth true

crush mcp list
crush mcp remove rocketbox
  `,
}

var mcpAddCmd = &cobra.Command{
	Use:   "add <name> [flags]",
	Short: "Add or update an MCP server",
	Long: `Add an MCP server, or update the one with the same name.

Flags (the same as the crushrc "mcp add" builtin):
      --type string              stdio, sse, or http (default "stdio")
      --command string           executable for stdio servers
      --args string              command argument (repeatable)
      --env KEY VALUE            environment variable (repeatable)
      --url string               URL for HTTP/SSE servers
      --header KEY VALUE         HTTP header (repeatable)
      --timeout int              startup timeout in seconds
      --disabled bool            disable without removing
      --disabled-tools string    deny a server tool (repeatable)
      --enabled-tools string     allow only these server tools (repeatable)
      --sessionless bool         the server cannot hold a session
      --oauth bool               enable the OAuth 2.1 flow (HTTP only)
      --oauth-client-id string   pre-registered OAuth client ID
      --oauth-client-secret string  pre-registered OAuth client secret
      --oauth-callback-port int  fixed localhost port for the OAuth callback
      --workspace                write to this project's .crush/crush.json`,
	// The server flags are parsed by the shared shellconfig parser so they
	// mean exactly what they mean in a crushrc; cobra only sees --workspace.
	DisableFlagParsing: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		workspaceScope, args := splitWorkspaceFlag(args)
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			return errors.New("usage: crush mcp add <name> [flags]")
		}
		name := args[0]
		if err := validateMCPName(name); err != nil {
			return err
		}
		server, err := shellconfig.MCPServerFromArgs(args[1:])
		if err != nil {
			return err
		}
		if err := validateMCPServer(server); err != nil {
			return err
		}

		c, ws, cleanup, err := connectToServer(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		scope := scopeFor(workspaceScope)
		if err := c.SetConfigField(getMCPContext(), ws.ID, scope, "mcp."+name, server); err != nil {
			return err
		}
		fmt.Printf("MCP server %q saved to the %s config.\n", name, scope)
		fmt.Println("A running Crush picks it up from the MCP Servers dialog, or on its next start.")
		return nil
	},
}

var mcpRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm"},
	Short:   "Remove an MCP server",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if err := validateMCPName(name); err != nil {
			return err
		}
		workspaceScope, _ := cmd.Flags().GetBool("workspace")

		c, ws, cleanup, err := connectToServer(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		ctx := getMCPContext()
		cfg, err := c.GetConfig(ctx, ws.ID)
		if err != nil {
			return fmt.Errorf("failed to get config: %w", err)
		}
		if _, ok := cfg.MCP[name]; !ok {
			return fmt.Errorf("no MCP server named %q is configured", name)
		}
		scope := scopeFor(workspaceScope)
		if err := c.RemoveConfigField(ctx, ws.ID, scope, "mcp."+name); err != nil {
			return err
		}
		fmt.Printf("MCP server %q removed from the %s config.\n", name, scope)
		return nil
	},
}

var mcpListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List configured MCP servers",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, ws, cleanup, err := connectToServer(cmd)
		if err != nil {
			return err
		}
		defer cleanup()

		cfg, err := c.GetConfig(getMCPContext(), ws.ID)
		if err != nil {
			return fmt.Errorf("failed to get config: %w", err)
		}
		servers := cfg.MCP.Sorted()
		if len(servers) == 0 {
			fmt.Println("No MCP servers are configured. Add one with: crush mcp add <name> [flags]")
			return nil
		}
		for _, s := range servers {
			fmt.Println(describeMCP(s.Name, s.MCP))
		}
		return nil
	},
}

// describeMCP renders one server as a single line: name, type, where it
// runs or connects, and anything that changes how it is used.
func describeMCP(name string, m config.MCPConfig) string {
	typ := cmp.Or(string(m.Type), "stdio")
	target := m.URL
	if typ == "stdio" {
		target = strings.TrimSpace(m.Command + " " + strings.Join(m.Args, " "))
	}
	var notes []string
	if m.Disabled {
		notes = append(notes, "disabled")
	}
	if m.OAuth {
		notes = append(notes, "oauth")
	}
	if len(m.Headers) > 0 {
		notes = append(notes, fmt.Sprintf("%d header(s)", len(m.Headers)))
	}
	line := fmt.Sprintf("%-20s %-6s %s", name, typ, target)
	if len(notes) > 0 {
		line += "  [" + strings.Join(notes, ", ") + "]"
	}
	return line
}

// validateMCPName keeps the name usable as a config key. The config store
// addresses fields by JSON path, where "." descends and "*" and "?" match,
// so a name carrying them would land somewhere other than mcp.<name>.
func validateMCPName(name string) error {
	if name == "" {
		return errors.New("the MCP server needs a name")
	}
	if strings.ContainsAny(name, ".*?|#@\\ \t\n") {
		return fmt.Errorf("invalid MCP server name %q: use letters, digits, - and _", name)
	}
	return nil
}

// validateMCPServer refuses the two definitions that would only fail later at
// connect time, with the reason they would fail.
func validateMCPServer(m map[string]any) error {
	typ, _ := m["type"].(string)
	switch typ {
	case "stdio":
		if cmd, _ := m["command"].(string); cmd == "" {
			return errors.New("a stdio server needs --command")
		}
	case "http", "sse":
		if u, _ := m["url"].(string); u == "" {
			return fmt.Errorf("a %s server needs --url", typ)
		}
	default:
		return fmt.Errorf("unknown --type %q: expected stdio, http or sse", typ)
	}
	return nil
}

// splitWorkspaceFlag pulls --workspace out of the raw argument list, since
// flag parsing is disabled on the add command so the server flags reach the
// shared parser untouched.
func splitWorkspaceFlag(args []string) (bool, []string) {
	var (
		workspace bool
		rest      = make([]string, 0, len(args))
	)
	for _, a := range args {
		if a == "--workspace" {
			workspace = true
			continue
		}
		rest = append(rest, a)
	}
	return workspace, rest
}

func scopeFor(workspace bool) config.Scope {
	if workspace {
		return config.ScopeWorkspace
	}
	return config.ScopeGlobal
}

func getMCPContext() context.Context {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	go func() {
		<-ctx.Done()
		cancel()
	}()
	return ctx
}

func init() {
	mcpRemoveCmd.Flags().Bool("workspace", false, "Remove from this project's .crush/crush.json instead of the global config")
	mcpCmd.AddCommand(mcpAddCmd, mcpRemoveCmd, mcpListCmd)
}
