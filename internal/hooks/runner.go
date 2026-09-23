package hooks

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/shell"
)

// abandonGrace is how long runOne waits after ctx cancellation for the
// shell goroutine to yield before returning control to the caller and
// letting the goroutine finish on its own. Mirrors the historical
// cmd.WaitDelay = time.Second behavior of the previous os/exec path.
const abandonGrace = time.Second

// asyncTimeout bounds a background hook that declared no timeout of its own.
const asyncTimeout = 5 * time.Minute

// runShell is the shell executor used by runOne. It is a package-level
// variable so tests can substitute a blocking or non-yielding
// implementation to exercise the abandon-on-timeout path without
// depending on the scheduling behavior of the real interpreter.
var runShell = shell.Run

// compiledHook pairs a HookConfig with its compiled matcher regex. A nil
// matcher means "match every subject".
type compiledHook struct {
	cfg     config.HookConfig
	matcher *regexp.Regexp
}

// Runner executes hook commands and aggregates their results.
type Runner struct {
	byEvent    map[string][]compiledHook
	cwd        string
	projectDir string
}

// NewRunner creates a Runner whose hooks all fire on PreToolUse. It is the
// single-event form kept for callers and tests that predate the other
// events; NewEventRunner is the general constructor.
func NewRunner(hooks []config.HookConfig, cwd, projectDir string) *Runner {
	return NewEventRunner(map[string][]config.HookConfig{EventPreToolUse: hooks}, cwd, projectDir)
}

// NewEventRunner creates a Runner from hooks keyed by event name, as in
// Config.Hooks. Each hook's Matcher is compiled here so the Runner is
// self-sufficient; callers do not have to pre-compile matchers on the
// config, and reloads or merges that rebuild HookConfig values can't
// silently strip compiled state.
//
// Hooks whose matcher fails to compile are skipped with a warning rather
// than treated as match-everything. ValidateHooks is expected to have
// caught syntax errors earlier, so this is defense in depth.
func NewEventRunner(hooks map[string][]config.HookConfig, cwd, projectDir string) *Runner {
	byEvent := make(map[string][]compiledHook, len(hooks))
	for event, list := range hooks {
		compiled := make([]compiledHook, 0, len(list))
		for _, h := range list {
			ch := compiledHook{cfg: h}
			if h.Matcher != "" {
				re, err := regexp.Compile(h.Matcher)
				if err != nil {
					slog.Warn(
						"Hook matcher failed to compile; skipping hook",
						"event", event,
						"matcher", h.Matcher,
						"command", h.Command,
						"error", err,
					)
					continue
				}
				ch.matcher = re
			}
			compiled = append(compiled, ch)
		}
		if len(compiled) > 0 {
			byEvent[event] = compiled
		}
	}
	return &Runner{
		byEvent:    byEvent,
		cwd:        cwd,
		projectDir: projectDir,
	}
}

// Has reports whether any hook is configured for the event, so callers can
// skip building an Event payload nobody will read.
func (r *Runner) Has(event string) bool {
	return r != nil && len(r.byEvent[event]) > 0
}

// Hooks returns the PreToolUse hook configs the runner was created with,
// in config order. Hooks whose matcher failed to compile at construction
// are omitted. Intended for diagnostics; callers should not rely on
// ordering or identity beyond that.
func (r *Runner) Hooks() []config.HookConfig {
	return r.HooksFor(EventPreToolUse)
}

// HooksFor returns the hook configs for one event, in config order.
func (r *Runner) HooksFor(event string) []config.HookConfig {
	list := r.byEvent[event]
	out := make([]config.HookConfig, len(list))
	for i, h := range list {
		out[i] = h.cfg
	}
	return out
}

// Run executes all matching hooks for a tool event, returning an
// aggregated result. It is the tool-shaped entry point; RunEvent takes
// any Event.
func (r *Runner) Run(ctx context.Context, eventName, sessionID, toolName, toolInputJSON string) (AggregateResult, error) {
	return r.RunEvent(ctx, Event{
		Name:      eventName,
		SessionID: sessionID,
		MatchKey:  toolName,
		ToolName:  toolName,
		ToolInput: toolInputJSON,
	})
}

// RunEvent executes all hooks configured for ev.Name whose matcher accepts
// ev.MatchKey. Synchronous hooks run in parallel and their results compose
// in config order; async hooks are started and forgotten.
func (r *Runner) RunEvent(ctx context.Context, ev Event) (AggregateResult, error) {
	matching := r.matchingHooks(ev.Name, ev.MatchKey)
	if len(matching) == 0 {
		return AggregateResult{Decision: DecisionNone}, nil
	}

	// Deduplicate by command string.
	seen := make(map[string]bool, len(matching))
	var sync_, async []config.HookConfig
	for _, h := range matching {
		if seen[h.Command] {
			continue
		}
		seen[h.Command] = true
		if h.Async {
			async = append(async, h)
		} else {
			sync_ = append(sync_, h)
		}
	}

	envVars := BuildEnv(ev.Name, ev.ToolName, ev.SessionID, r.cwd, r.projectDir, ev.ToolInput)
	payload := BuildEventPayload(ev, r.cwd)
	plainContext := plainTextIsContext(ev.Name)

	for _, h := range async {
		go func(hook config.HookConfig) {
			timeout := asyncTimeout
			if hook.Timeout > 0 {
				timeout = hook.TimeoutDuration()
			}
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			defer cancel()
			r.runOne(actx, hook, envVars, payload, plainContext)
		}(h)
	}

	results := make([]HookResult, len(sync_))
	var wg sync.WaitGroup
	wg.Add(len(sync_))
	for i, h := range sync_ {
		go func(idx int, hook config.HookConfig) {
			defer wg.Done()
			results[idx] = r.runOne(ctx, hook, envVars, payload, plainContext)
		}(i, h)
	}
	wg.Wait()

	agg := aggregate(results, ev.ToolInput)
	agg.Hooks = make([]HookInfo, 0, len(sync_)+len(async))
	for i, h := range sync_ {
		agg.Hooks = append(agg.Hooks, HookInfo{
			Name:         h.DisplayName(),
			Matcher:      h.Matcher,
			Decision:     results[i].Decision.String(),
			Halt:         results[i].Halt,
			Reason:       results[i].Reason,
			InputRewrite: results[i].UpdatedInput != "",
		})
	}
	for _, h := range async {
		agg.Hooks = append(agg.Hooks, HookInfo{Name: h.DisplayName(), Matcher: h.Matcher, Decision: DecisionNone.String()})
	}
	agg.HookCount = len(agg.Hooks)
	slog.Info(
		"Hook completed",
		"event", ev.Name,
		"subject", ev.MatchKey,
		"hooks", agg.HookCount,
		"decision", agg.Decision.String(),
	)
	return agg, nil
}

// plainTextIsContext reports whether non-JSON stdout from a hook on this
// event is fed to the model as context, which is how Claude Code treats
// UserPromptSubmit and SessionStart output.
func plainTextIsContext(event string) bool {
	return event == EventUserPromptSubmit || event == EventSessionStart
}

// matchingHooks returns the event's hooks whose matcher matches subject (or
// has no matcher, which matches everything).
func (r *Runner) matchingHooks(event, subject string) []config.HookConfig {
	var matched []config.HookConfig
	for _, h := range r.byEvent[event] {
		if h.matcher == nil || h.matcher.MatchString(subject) {
			matched = append(matched, h.cfg)
		}
	}
	return matched
}

// runOne executes a single hook command and returns its result.
//
// Execution goes through Crush's embedded POSIX shell (shell.Run) so the
// same interpreter, builtins, and coreutils are visible to hooks as to
// the bash tool. BlockFuncs are intentionally omitted: hooks are
// user-authored config that carry the same trust as a shell alias.
//
// A hook that fails to yield after its deadline has passed is abandoned
// after abandonGrace so the caller never blocks longer than
// timeout + abandonGrace. Ownership of the stdout and stderr buffers is
// strictly single-goroutine:
//   - before receiving from `done`, only the goroutine writes to them;
//   - after `done` delivers a value, the goroutine is finished and the
//     outer frame reads them;
//   - on the abandon path, the goroutine may still be writing and the
//     outer frame must not touch them again.
func (r *Runner) runOne(parentCtx context.Context, hook config.HookConfig, envVars []string, payload []byte, plainContext bool) HookResult {
	timeout := hook.TimeoutDuration()
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runShell(ctx, shell.RunOptions{
			Command: hook.Command,
			Cwd:     r.cwd,
			Env:     envVars,
			Stdin:   bytes.NewReader(payload),
			Stdout:  &stdout,
			Stderr:  &stderr,
		})
	}()

	var err error
	select {
	case err = <-done:
		// Normal path: goroutine has finished, buffers are safe to read.
	case <-ctx.Done():
		select {
		case err = <-done:
			// Interpreter yielded within the grace period; safe to read.
		case <-time.After(abandonGrace):
			slog.Warn(
				"Hook did not yield after cancel; abandoning goroutine",
				"command", hook.Command,
				"timeout", timeout,
			)
			// The goroutine may still be writing to stdout/stderr; do
			// not read either buffer below this point.
			return HookResult{Decision: DecisionNone}
		}
	}

	if shell.IsInterrupt(err) {
		// Distinguish timeout from parent cancellation.
		if parentCtx.Err() != nil {
			slog.Debug("Hook cancelled by parent context", "command", hook.Command)
		} else {
			slog.Warn("Hook timed out", "command", hook.Command, "timeout", timeout)
		}
		return HookResult{Decision: DecisionNone}
	}

	if err != nil {
		exitCode := shell.ExitCode(err)
		switch exitCode {
		case 2:
			// Exit code 2 = block. Stderr is the reason, and the caller
			// decides what blocking means for its event.
			reason := strings.TrimSpace(stderr.String())
			if reason == "" {
				reason = "blocked by hook"
			}
			return HookResult{
				Decision: DecisionDeny,
				Reason:   reason,
			}
		case HaltExitCode:
			// Exit code 49 = halt the whole turn. Stderr is the reason.
			reason := strings.TrimSpace(stderr.String())
			if reason == "" {
				reason = "turn halted by hook"
			}
			return HookResult{
				Decision: DecisionDeny,
				Halt:     true,
				Reason:   reason,
			}
		default:
			// Other non-zero exits are non-blocking errors.
			slog.Warn(
				"Hook failed with non-blocking error",
				"command", hook.Command,
				"exit_code", exitCode,
				"stderr", strings.TrimSpace(stderr.String()),
				"error", err,
			)
			return HookResult{Decision: DecisionNone}
		}
	}

	// Exit code 0 — parse stdout.
	result := parseStdoutFor(stdout.String(), plainContext)
	slog.Debug(
		"Hook executed",
		"command", hook.Command,
		"decision", result.Decision.String(),
	)
	return result
}
