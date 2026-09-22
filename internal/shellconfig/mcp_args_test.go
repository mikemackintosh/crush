package shellconfig

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPServerFromArgs(t *testing.T) {
	t.Parallel()

	m, err := MCPServerFromArgs([]string{
		"--type", "http", "--url", "https://rocketbox.ai/mcp",
		"--header", "Authorization", "Bearer x", "--oauth", "true",
		"--disabled-tools", "a", "--disabled-tools", "b", "--timeout", "15",
	})
	require.NoError(t, err)
	require.Equal(t, "http", m["type"])
	require.Equal(t, "https://rocketbox.ai/mcp", m["url"])
	require.Equal(t, map[string]any{"Authorization": "Bearer x"}, m["headers"])
	require.Equal(t, true, m["oauth"])
	require.Equal(t, []any{"a", "b"}, m["disabled_tools"])
	require.Equal(t, int64(15), m["timeout"])

	m, err = MCPServerFromArgs([]string{"--command", "npx", "--args", "-y", "--args", "srv", "--env", "K", "V"})
	require.NoError(t, err)
	require.Equal(t, "stdio", m["type"], "stdio is the default, as in a crushrc")
	require.Equal(t, []any{"-y", "srv"}, m["args"])
	require.Equal(t, map[string]any{"K": "V"}, m["env"])

	_, err = MCPServerFromArgs([]string{"--bogus", "1"})
	require.ErrorContains(t, err, "unknown flag --bogus")

	_, err = MCPServerFromArgs([]string{"--timeout", "soon"})
	require.ErrorContains(t, err, "expects an integer")
}
