package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
)

// The lifecycle hook events other than the two tool events live here.
// Each fires through the same Runner the tool wrapper uses, so a hook's
// matcher, timeout, async flag and output contract mean the same thing on
// every event.

// sessionEndTimeout bounds SessionEnd hooks, which run while the process
// is shutting down.
const sessionEndTimeout = 5 * time.Second

// hooksRunner returns the current hook runner, or nil when no hooks are
// configured. It is rebuilt whenever the main agent's tools are.
func (c *coordinator) hooksRunner() *hooks.Runner {
	return c.hookRunner.Load()
}

// fireSessionStart runs SessionStart hooks the first time a session is
// used in this process. source is "startup" for a fresh session and
// "resume" for one that already holds messages. Context the hooks return
// is handed back so the caller can add it to the first prompt.
func (c *coordinator) fireSessionStart(ctx context.Context, sessionID string) string {
	if _, seen := c.hookSessions.LoadOrStore(sessionID, struct{}{}); seen {
		return ""
	}
	r := c.hooksRunner()
	if !r.Has(hooks.EventSessionStart) {
		return ""
	}
	source := "startup"
	if msgs, err := c.messages.List(ctx, sessionID); err == nil && len(msgs) > 0 {
		source = "resume"
	}
	res, err := r.RunEvent(ctx, hooks.Event{
		Name:      hooks.EventSessionStart,
		SessionID: sessionID,
		MatchKey:  source,
		Fields:    map[string]any{"source": source},
	})
	if err != nil {
		slog.Warn("SessionStart hook error", "error", err)
		return ""
	}
	c.reportHookMessage(res)
	return res.Context
}

// fireUserPromptSubmit runs UserPromptSubmit hooks. A block refuses the
// prompt with the hook's reason; otherwise any context the hooks return
// is appended to the prompt the model sees.
func (c *coordinator) fireUserPromptSubmit(ctx context.Context, sessionID, prompt string) (string, error) {
	r := c.hooksRunner()
	if !r.Has(hooks.EventUserPromptSubmit) {
		return "", nil
	}
	res, err := r.RunEvent(ctx, hooks.Event{
		Name:      hooks.EventUserPromptSubmit,
		SessionID: sessionID,
		Fields:    map[string]any{"prompt": prompt},
	})
	if err != nil {
		slog.Warn("UserPromptSubmit hook error", "error", err)
		return "", nil
	}
	c.reportHookMessage(res)
	if res.Blocked() {
		return "", &hooks.BlockedError{Event: hooks.EventUserPromptSubmit, Reason: res.Reason}
	}
	return res.Context, nil
}

// withHookContext appends hook-provided context to a prompt.
func withHookContext(prompt, context string) string {
	context = strings.TrimSpace(context)
	if context == "" {
		return prompt
	}
	return prompt + "\n\n<hook-context>\n" + context + "\n</hook-context>"
}

// stopHookActiveKey marks a context as a continuation forced by a Stop
// hook, which the hook payload reports as stop_hook_active.
type stopHookActiveKey struct{}

func stopHookActive(ctx context.Context) bool {
	v, _ := ctx.Value(stopHookActiveKey{}).(bool)
	return v
}

// runStopLoop runs Stop (or SubagentStop) hooks after a turn and, while a
// hook blocks the stop, sends the agent back to work with the hook's reason
// as its next prompt. run must read prompt and ctx through the pointers so
// each continuation sees the new values. Continuations are hidden user
// messages and capped by hooks.MaxStopContinuations.
func (c *coordinator) runStopLoop(
	event string,
	ctx *context.Context,
	sessionID string,
	prompt *string,
	run func() (*fantasy.AgentResult, error),
	result *fantasy.AgentResult,
	runErr error,
) (*fantasy.AgentResult, error) {
	r := c.hooksRunner()
	if runErr != nil || !r.Has(event) {
		return result, runErr
	}
	for i := 0; i < hooks.MaxStopContinuations; i++ {
		res, err := r.RunEvent(*ctx, hooks.Event{
			Name:      event,
			SessionID: sessionID,
			Fields: map[string]any{
				"stop_hook_active": stopHookActive(*ctx),
				"last_response":    subAgentOutput(result),
			},
		})
		if err != nil {
			slog.Warn("Stop hook error", "event", event, "error", err)
			return result, runErr
		}
		c.reportHookMessage(res)
		if res.Halt || res.Decision != hooks.DecisionDeny {
			// Allowed to stop, or halted outright: either way the turn ends.
			return result, runErr
		}
		reason := res.Reason
		if reason == "" {
			reason = "A hook asked you to continue working."
		}
		slog.Info("Stop hook blocked the stop; continuing", "event", event, "reason", reason, "continuation", i+1)
		*prompt = reason
		*ctx = message.WithHiddenUserMessage(context.WithValue(*ctx, stopHookActiveKey{}, true))
		result, runErr = run()
		if runErr != nil {
			return result, runErr
		}
	}
	slog.Warn("Stop hook kept blocking; giving up after the cap", "event", event, "cap", hooks.MaxStopContinuations)
	return result, runErr
}

// firePreCompact runs PreCompact hooks. trigger is "manual" for a user
// request and "auto" when the agent compacts on its own. Only a halt
// aborts the compaction; PreCompact is otherwise informational.
func (c *coordinator) firePreCompact(ctx context.Context, sessionID, trigger string) error {
	r := c.hooksRunner()
	if !r.Has(hooks.EventPreCompact) {
		return nil
	}
	res, err := r.RunEvent(ctx, hooks.Event{
		Name:      hooks.EventPreCompact,
		SessionID: sessionID,
		MatchKey:  trigger,
		Fields:    map[string]any{"trigger": trigger, "custom_instructions": ""},
	})
	if err != nil {
		slog.Warn("PreCompact hook error", "error", err)
		return nil
	}
	c.reportHookMessage(res)
	if res.Halt {
		return &hooks.BlockedError{Event: hooks.EventPreCompact, Reason: res.Reason}
	}
	return nil
}

// fireNotification runs Notification hooks. kind is the matcher subject
// and the notification_type field; message is free text. Fire and forget:
// nothing a notification hook returns changes what the agent does.
func (c *coordinator) fireNotification(ctx context.Context, sessionID, kind, message string) {
	r := c.hooksRunner()
	if !r.Has(hooks.EventNotification) {
		return
	}
	go func() {
		nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, err := r.RunEvent(nctx, hooks.Event{
			Name:      hooks.EventNotification,
			SessionID: sessionID,
			MatchKey:  kind,
			Fields:    map[string]any{"notification_type": kind, "message": message},
		}); err != nil {
			slog.Warn("Notification hook error", "kind", kind, "error", err)
		}
	}()
}

// SessionEnder is implemented by the coordinator; the app shutdown looks
// for it rather than widening the Coordinator interface every fake must
// satisfy.
type SessionEnder interface {
	SessionEnd(ctx context.Context, reason string)
}

// SessionEnd runs SessionEnd hooks for every session this process ran, with
// the given reason. Called on shutdown; bounded so a slow hook cannot hold
// the exit.
func (c *coordinator) SessionEnd(ctx context.Context, reason string) {
	r := c.hooksRunner()
	if !r.Has(hooks.EventSessionEnd) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionEndTimeout)
	defer cancel()
	c.hookSessions.Range(func(key, _ any) bool {
		sessionID, _ := key.(string)
		if _, err := r.RunEvent(ctx, hooks.Event{
			Name:      hooks.EventSessionEnd,
			SessionID: sessionID,
			MatchKey:  reason,
			Fields:    map[string]any{"reason": reason},
		}); err != nil {
			slog.Warn("SessionEnd hook error", "session", sessionID, "error", err)
		}
		return true
	})
}

// watchPermissionPrompts turns every permission prompt into a
// Notification hook event of type permission_prompt, the way Claude Code
// notifies when it is waiting on the user.
func (c *coordinator) watchPermissionPrompts(ctx context.Context) {
	if c.permissions == nil {
		return
	}
	ch := c.permissions.Subscribe(ctx)
	go func() {
		for ev := range ch {
			if ev.Type != pubsub.CreatedEvent {
				continue
			}
			req := ev.Payload
			c.fireNotification(ctx, req.SessionID, "permission_prompt",
				fmt.Sprintf("%s needs permission: %s", req.ToolName, req.Description))
		}
	}()
}

// reportHookMessage surfaces a hook's systemMessage to the user through
// the log; the tool path carries it in the response metadata instead.
func (c *coordinator) reportHookMessage(res hooks.AggregateResult) {
	if res.SystemMessage != "" {
		slog.Info("Hook message", "message", res.SystemMessage)
	}
}
