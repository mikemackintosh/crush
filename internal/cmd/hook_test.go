package cmd

import (
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestHookCmd_Subcommands(t *testing.T) {
	t.Parallel()
	names := map[string]bool{}
	for _, c := range hookCmd.Commands() {
		names[c.Name()] = true
	}
	require.True(t, names["add"])
	require.True(t, names["remove"])
	require.True(t, names["list"])
	require.True(t, hookAddCmd.DisableFlagParsing, "add hands its flags to the shared crushrc parser")
}

func TestCanonicalHookEvent(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"PreToolUse": "PreToolUse", "pre_tool_use": "PreToolUse", "pretooluse": "PreToolUse",
		"user-prompt-submit": "UserPromptSubmit", "STOP": "Stop", "SessionEnd": "SessionEnd",
	} {
		got, ok := canonicalHookEvent(in)
		require.True(t, ok, in)
		require.Equal(t, want, got, in)
	}
	_, ok := canonicalHookEvent("OnCoffee")
	require.False(t, ok)
}

func TestHookMapsAndDescribe(t *testing.T) {
	t.Parallel()
	list := []config.HookConfig{
		{Command: "./a.sh"},
		{Name: "guard", Matcher: "^bash$", Command: "./b.sh", Timeout: 5, Async: true},
	}
	maps := hookMaps(list)
	require.Equal(t, []map[string]any{
		{"command": "./a.sh"},
		{"command": "./b.sh", "name": "guard", "matcher": "^bash$", "timeout": 5, "async": true},
	}, maps)

	line := describeHook(list[1])
	require.Contains(t, line, "guard")
	require.Contains(t, line, "matcher=^bash$")
	require.Contains(t, line, "timeout=5s")
	require.Contains(t, line, "async")
	require.Contains(t, line, "command=./b.sh")
	require.Equal(t, "./a.sh", describeHook(list[0]))
}
