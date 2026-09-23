package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

func newHookedCoordinator(t *testing.T, byEvent map[string][]config.HookConfig) *coordinator {
	t.Helper()
	c := &coordinator{}
	c.hookRunner.Store(hooks.NewEventRunner(byEvent, t.TempDir(), t.TempDir()))
	return c
}

// A Stop hook that blocks sends the agent back to work with its reason as
// the next prompt, hidden from the transcript, until it stops blocking or
// the cap is hit.
func TestRunStopLoop_ContinuesUntilHookAllows(t *testing.T) {
	t.Parallel()
	// Block the first two stops, allow the third.
	dir := t.TempDir()
	c := newHookedCoordinator(t, map[string][]config.HookConfig{
		hooks.EventStop: {{Command: `n=$(cat "` + dir + `/n" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "` + dir + `/n"; if [ $n -le 2 ]; then echo '{"decision":"block","reason":"finish the tests"}'; fi`}},
	})

	var calls atomic.Int32
	var seenPrompts []string
	var seenHidden []bool
	ctx := context.Background()
	prompt := "original"
	run := func() (*fantasy.AgentResult, error) {
		calls.Add(1)
		seenPrompts = append(seenPrompts, prompt)
		seenHidden = append(seenHidden, message.HiddenUserMessage(ctx))
		return &fantasy.AgentResult{}, nil
	}

	result, err := c.runStopLoop(hooks.EventStop, &ctx, "s1", &prompt, run, &fantasy.AgentResult{}, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, int32(2), calls.Load(), "two continuations, then the hook let it stop")
	require.Equal(t, []string{"finish the tests", "finish the tests"}, seenPrompts)
	require.Equal(t, []bool{true, true}, seenHidden, "continuations are hidden user messages")
	require.True(t, stopHookActive(ctx), "the context reports stop_hook_active for later hooks")
}

func TestRunStopLoop_CapsRunawayHooks(t *testing.T) {
	t.Parallel()
	c := newHookedCoordinator(t, map[string][]config.HookConfig{
		hooks.EventStop: {{Command: `echo '{"decision":"block","reason":"again"}'`}},
	})
	var calls int
	ctx := context.Background()
	prompt := "p"
	run := func() (*fantasy.AgentResult, error) { calls++; return &fantasy.AgentResult{}, nil }
	_, err := c.runStopLoop(hooks.EventStop, &ctx, "s1", &prompt, run, &fantasy.AgentResult{}, nil)
	require.NoError(t, err)
	require.Equal(t, hooks.MaxStopContinuations, calls)
}

func TestRunStopLoop_SkipsOnErrorOrNoHooks(t *testing.T) {
	t.Parallel()
	c := newHookedCoordinator(t, map[string][]config.HookConfig{})
	ctx := context.Background()
	prompt := "p"
	ran := false
	run := func() (*fantasy.AgentResult, error) { ran = true; return nil, nil }
	_, err := c.runStopLoop(hooks.EventStop, &ctx, "s1", &prompt, run, nil, nil)
	require.NoError(t, err)
	require.False(t, ran)

	c = newHookedCoordinator(t, map[string][]config.HookConfig{hooks.EventStop: {{Command: `echo '{"decision":"block"}'`}}})
	boom := errors.New("boom")
	_, err = c.runStopLoop(hooks.EventStop, &ctx, "s1", &prompt, run, nil, boom)
	require.ErrorIs(t, err, boom, "a failed turn is not continued")
	require.False(t, ran)
}

func TestFireUserPromptSubmit(t *testing.T) {
	t.Parallel()
	c := newHookedCoordinator(t, map[string][]config.HookConfig{
		hooks.EventUserPromptSubmit: {{Command: `if grep -q secret; then echo "no secrets" >&2; exit 2; fi; echo "branch: main"`}},
	})
	// The payload arrives on stdin; grep reads it there.
	ctxs, err := c.fireUserPromptSubmit(context.Background(), "s1", "hello")
	require.NoError(t, err)
	require.Equal(t, "branch: main", ctxs)

	_, err = c.fireUserPromptSubmit(context.Background(), "s1", "the secret is 42")
	var blocked *hooks.BlockedError
	require.ErrorAs(t, err, &blocked)
	require.Equal(t, "no secrets", blocked.Reason)

	require.Equal(t, "hi\n\n<hook-context>\nbranch: main\n</hook-context>", withHookContext("hi", "branch: main"))
	require.Equal(t, "hi", withHookContext("hi", "  "))
}

func TestFirePreCompact_OnlyHaltAborts(t *testing.T) {
	t.Parallel()
	c := newHookedCoordinator(t, map[string][]config.HookConfig{
		hooks.EventPreCompact: {
			{Matcher: "^manual$", Command: `echo "nope" >&2; exit 2`},
			{Matcher: "^auto$", Command: `echo "never" >&2; exit 49`},
		},
	})
	require.NoError(t, c.firePreCompact(context.Background(), "s1", "manual"), "a plain block is informational")
	err := c.firePreCompact(context.Background(), "s1", "auto")
	var blocked *hooks.BlockedError
	require.ErrorAs(t, err, &blocked)
	require.Equal(t, "never", blocked.Reason)
}
