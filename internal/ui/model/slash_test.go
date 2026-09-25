package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSlashCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		name, arg string
		ok        bool
	}{
		{"/mcp", "mcp", "", true},
		{"  /MCP rocketbox  ", "mcp", "rocketbox", true},
		{"/mcp   two words", "mcp", "two words", true},
		{"mcp", "", "", false},
		{"/", "", "", false},
		{"/mcp\nand more", "", "", false},
		{"/etc/hosts is where?", "", "", false},
		{"/model", "model", "", true},
		{"/models", "models", "", true},
		{"/login llmgw", "login", "llmgw", true},
		{"/123", "", "", false},
	}
	for _, tc := range cases {
		name, arg, ok := parseSlashCommand(tc.in)
		require.Equal(t, tc.ok, ok, tc.in)
		require.Equal(t, tc.name, name, tc.in)
		require.Equal(t, tc.arg, arg, tc.in)
	}
}
