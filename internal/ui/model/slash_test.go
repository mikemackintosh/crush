package model

import (
	"testing"

	"github.com/charmbracelet/crush/internal/session"
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
		{"/fork Try the other approach", "fork", "Try the other approach", true},
		{"/sessions", "sessions", "", true},
		{"/forks", "forks", "", true},
		{"/123", "", "", false},
	}
	for _, tc := range cases {
		name, arg, ok := parseSlashCommand(tc.in)
		require.Equal(t, tc.ok, ok, tc.in)
		require.Equal(t, tc.name, name, tc.in)
		require.Equal(t, tc.arg, arg, tc.in)
	}
}

func TestForkSwitchTarget(t *testing.T) {
	t.Parallel()
	src := session.Session{ID: "src", Title: "Source"}
	newer := session.Session{ID: "f2", Title: "Fork 2", ForkedFrom: "src"}
	older := session.Session{ID: "f1", Title: "Fork 1", ForkedFrom: "src"}
	other := session.Session{ID: "x", Title: "Unrelated"}
	list := []session.Session{newer, older, src, other} // newest first

	got, ok := forkSwitchTarget(src, list)
	require.True(t, ok)
	require.Equal(t, "f2", got.ID, "a source jumps to its newest fork")

	got, ok = forkSwitchTarget(older, list)
	require.True(t, ok)
	require.Equal(t, "src", got.ID, "a fork jumps back to its source")

	_, ok = forkSwitchTarget(other, list)
	require.False(t, ok)
}
