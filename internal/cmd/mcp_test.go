package cmd

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestMCPCmd_Subcommands(t *testing.T) {
	t.Parallel()
	names := map[string]bool{}
	for _, c := range mcpCmd.Commands() {
		names[c.Name()] = true
	}
	require.True(t, names["add"])
	require.True(t, names["remove"])
	require.True(t, names["list"])
	require.Equal(t, []string{"rm"}, mcpRemoveCmd.Aliases)
	require.True(t, mcpAddCmd.DisableFlagParsing, "add hands its flags to the shared crushrc parser")
}

func TestValidateMCPName(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"github", "my-server", "srv_2"} {
		require.NoError(t, validateMCPName(ok), ok)
	}
	for _, bad := range []string{"", "a.b", "a b", "a*", "a?"} {
		require.Error(t, validateMCPName(bad), bad)
	}
}

func TestValidateMCPServer(t *testing.T) {
	t.Parallel()
	require.NoError(t, validateMCPServer(map[string]any{"type": "stdio", "command": "npx"}))
	require.NoError(t, validateMCPServer(map[string]any{"type": "http", "url": "https://x.example/mcp"}))
	require.ErrorContains(t, validateMCPServer(map[string]any{"type": "stdio"}), "--command")
	require.ErrorContains(t, validateMCPServer(map[string]any{"type": "sse"}), "--url")
	require.ErrorContains(t, validateMCPServer(map[string]any{"type": "grpc", "url": "x"}), "unknown --type")
}

func TestSplitWorkspaceFlag(t *testing.T) {
	t.Parallel()
	ws, rest := splitWorkspaceFlag([]string{"srv", "--workspace", "--type", "http"})
	require.True(t, ws)
	require.Equal(t, []string{"srv", "--type", "http"}, rest)

	ws, rest = splitWorkspaceFlag([]string{"srv", "--type", "http"})
	require.False(t, ws)
	require.Equal(t, []string{"srv", "--type", "http"}, rest)
}

func TestDescribeMCP(t *testing.T) {
	t.Parallel()
	line := describeMCP("rocketbox", config.MCPConfig{Type: "http", URL: "https://rocketbox.ai/mcp", OAuth: true})
	require.Contains(t, line, "rocketbox")
	require.Contains(t, line, "http")
	require.Contains(t, line, "https://rocketbox.ai/mcp")
	require.Contains(t, line, "[oauth]")

	line = describeMCP("fs", config.MCPConfig{Command: "npx", Args: []string{"-y", "server"}, Disabled: true})
	require.Contains(t, line, "stdio")
	require.Contains(t, line, "npx -y server")
	require.Contains(t, line, "[disabled]")
}
