package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestEventRunnerMatchesOnSubject(t *testing.T) {
	t.Parallel()
	r := NewEventRunner(map[string][]config.HookConfig{
		EventSessionStart: {
			{Matcher: "^resume$", Command: `echo "welcome back"`},
			{Matcher: "^startup$", Command: `echo "fresh"`},
		},
		EventPreCompact: {{Command: `echo '{"systemMessage":"compacting"}'`}},
	}, t.TempDir(), t.TempDir())

	require.True(t, r.Has(EventSessionStart))
	require.True(t, r.Has(EventPreCompact))
	require.False(t, r.Has(EventStop))
	require.False(t, (*Runner)(nil).Has(EventStop), "a nil runner has no hooks")

	res, err := r.RunEvent(context.Background(), Event{Name: EventSessionStart, MatchKey: "resume", Fields: map[string]any{"source": "resume"}})
	require.NoError(t, err)
	require.Equal(t, 1, res.HookCount)
	require.Equal(t, "welcome back", res.Context, "plain stdout is context on SessionStart")

	res, err = r.RunEvent(context.Background(), Event{Name: EventPreCompact, MatchKey: "manual"})
	require.NoError(t, err)
	require.Equal(t, "compacting", res.SystemMessage)
	require.Equal(t, DecisionNone, res.Decision)
}

func TestEventPayloadCarriesClaudeCodeFields(t *testing.T) {
	t.Parallel()
	data := BuildEventPayload(Event{
		Name:      EventUserPromptSubmit,
		SessionID: "s1",
		Fields:    map[string]any{"prompt": "hello"},
	}, "/work")
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, EventUserPromptSubmit, got["hook_event_name"])
	require.Equal(t, EventUserPromptSubmit, got["event"])
	require.Equal(t, "hello", got["prompt"])
	require.Equal(t, "/work", got["cwd"])
	_, hasTool := got["tool_name"]
	require.False(t, hasTool, "non-tool events carry no tool fields")

	data = BuildEventPayload(Event{Name: EventPostToolUse, ToolName: "bash", ToolInput: `{"command":"ls"}`,
		Fields: map[string]any{"tool_response": map[string]any{"content": "a b", "is_error": false}}}, "/work")
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, "bash", got["tool_name"])
	require.Equal(t, map[string]any{"command": "ls"}, got["tool_input"])
	require.Equal(t, "a b", got["tool_response"].(map[string]any)["content"])
}

func TestParseStdoutClaudeCodeContract(t *testing.T) {
	t.Parallel()

	r := parseStdout(`{"decision":"block","reason":"not like that"}`)
	require.Equal(t, DecisionDeny, r.Decision)
	require.Equal(t, "not like that", r.Reason)

	r = parseStdout(`{"decision":"approve"}`)
	require.Equal(t, DecisionAllow, r.Decision)

	r = parseStdout(`{"continue":false,"stopReason":"budget exhausted","systemMessage":"told the user"}`)
	require.True(t, r.Halt)
	require.Equal(t, DecisionDeny, r.Decision)
	require.Equal(t, "budget exhausted", r.Reason)
	require.Equal(t, "told the user", r.SystemMessage)

	r = parseStdout(`{"continue":true,"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","additionalContext":"be careful"}}`)
	require.False(t, r.Halt)
	require.Equal(t, DecisionNone, r.Decision, "ask means the normal prompt")
	require.Equal(t, "be careful", r.Context)

	r = parseStdout(`{"context":"top","hookSpecificOutput":{"additionalContext":"specific","permissionDecision":"deny","permissionDecisionReason":"nope"}}`)
	require.Equal(t, DecisionDeny, r.Decision)
	require.Equal(t, "nope", r.Reason)
	require.Equal(t, "top\nspecific", r.Context)

	require.Equal(t, "", parseStdoutFor("just words", false).Context)
	require.Equal(t, "just words", parseStdoutFor("just words", true).Context)
}

func TestAsyncHookDoesNotBlockAndStillRuns(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "ran")
	r := NewEventRunner(map[string][]config.HookConfig{
		EventNotification: {{Async: true, Command: `sleep 0.3; echo hi > "` + marker + `"; exit 2`}},
	}, t.TempDir(), t.TempDir())

	start := time.Now()
	res, err := r.RunEvent(context.Background(), Event{Name: EventNotification, MatchKey: "agent_finished"})
	require.NoError(t, err)
	require.Less(t, time.Since(start), 250*time.Millisecond, "async hooks are not awaited")
	require.Equal(t, DecisionNone, res.Decision, "async output never becomes a decision")
	require.Equal(t, 1, res.HookCount)

	require.Eventually(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 3*time.Second, 50*time.Millisecond, "the async hook still ran to completion")
}

func TestBlockedError(t *testing.T) {
	t.Parallel()
	require.Equal(t, "UserPromptSubmit hook: no", (&BlockedError{Event: EventUserPromptSubmit, Reason: "no"}).Error())
	require.True(t, strings.Contains((&BlockedError{Event: EventPreCompact}).Error(), "blocked"))
}
