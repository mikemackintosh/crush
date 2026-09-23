// Package hooks runs user-defined shell commands that fire on hook events
// across the agent lifecycle, returning decisions that control agent
// behavior. The events, the stdin payload and the stdout contract follow
// Claude Code's hooks, so a hook written for one runs under the other.
package hooks

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/tidwall/sjson"
)

// Hook event name constants. See config.HookEvents for the full list.
const (
	EventPreToolUse       = config.HookEventPreToolUse
	EventPostToolUse      = config.HookEventPostToolUse
	EventUserPromptSubmit = config.HookEventUserPromptSubmit
	EventStop             = config.HookEventStop
	EventSubagentStop     = config.HookEventSubagentStop
	EventSessionStart     = config.HookEventSessionStart
	EventSessionEnd       = config.HookEventSessionEnd
	EventPreCompact       = config.HookEventPreCompact
	EventNotification     = config.HookEventNotification
)

// Event is one occurrence of a hook event: what fired, for which session,
// and the fields the hook receives on stdin. MatchKey is what a hook's
// matcher regex is tested against.
type Event struct {
	Name      string
	SessionID string
	// MatchKey is the matcher's subject: the tool name for tool events,
	// otherwise the source, reason, trigger or notification type.
	MatchKey string
	// ToolName and ToolInput are set for PreToolUse and PostToolUse.
	ToolName  string
	ToolInput string // JSON object
	// Fields are event-specific stdin fields (tool_response, prompt,
	// stop_hook_active, trigger, source, reason, message, ...).
	Fields map[string]any
}

// HaltExitCode is the exit code that halts the whole turn. 2 blocks the
// current tool call; 49 sits in the no-man's-land between the
// generic-error range (1-30), the sysexits range (64-78), and the
// killed-by-signal range (128+) so it can't be hit by accident.
const HaltExitCode = 49

// HookMetadata is embedded in tool response metadata so the UI can
// display a hook indicator.
type HookMetadata struct {
	HookCount    int        `json:"hook_count"`
	Decision     string     `json:"decision"`
	Halt         bool       `json:"halt,omitempty"`
	Reason       string     `json:"reason,omitempty"`
	InputRewrite bool       `json:"input_rewrite,omitempty"`
	Hooks        []HookInfo `json:"hooks,omitempty"`
}

// HookInfo identifies a single hook that ran and its individual result.
type HookInfo struct {
	Name         string `json:"name"`
	Matcher      string `json:"matcher,omitempty"`
	Decision     string `json:"decision"`
	Halt         bool   `json:"halt,omitempty"`
	Reason       string `json:"reason,omitempty"`
	InputRewrite bool   `json:"input_rewrite,omitempty"`
}

// Decision represents the outcome of a single hook execution.
type Decision int

const (
	// DecisionNone means the hook expressed no opinion.
	DecisionNone Decision = iota
	// DecisionAllow means the hook explicitly allowed the action.
	DecisionAllow
	// DecisionDeny means the hook blocked the action.
	DecisionDeny
)

func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	default:
		return "none"
	}
}

// HookResult holds the parsed output of a single hook execution.
type HookResult struct {
	Decision     Decision
	Halt         bool   // If true, halt the whole turn.
	Reason       string // Deny or halt reason (same field, different audience).
	Context      string
	UpdatedInput string // Shallow-merge patch against tool_input (opaque JSON).
	// SystemMessage is shown to the user, not the model (Claude Code's
	// systemMessage).
	SystemMessage string
}

// AggregateResult holds the combined outcome of all hooks for an event.
type AggregateResult struct {
	Decision      Decision
	Halt          bool       // Any hook requested halt.
	HookCount     int        // Number of hooks that ran.
	Hooks         []HookInfo // Info about each hook that ran (config order).
	Reason        string     // Concatenated deny/halt reasons (newline-separated).
	Context       string     // Concatenated context from all hooks.
	UpdatedInput  string     // Merged tool_input JSON (empty if no patches).
	SystemMessage string     // Concatenated messages for the user.
}

// Blocked reports whether the hooks refused the action, by deny or halt.
func (a AggregateResult) Blocked() bool {
	return a.Decision == DecisionDeny || a.Halt
}

// aggregate merges multiple HookResults into a single AggregateResult.
// Results are processed in config order (the order of the slice). Deny
// wins over allow, allow wins over none. Halt is sticky. Reasons and
// context concatenate in order. updated_input patches shallow-merge in
// order against the original tool input; later patches override earlier
// ones on colliding keys.
func aggregate(results []HookResult, origToolInput string) AggregateResult {
	var (
		decision Decision
		halt     bool
		reasons  []string
		contexts []string
		systems  []string
		merged   = origToolInput
		anyPatch = false
	)
	for _, r := range results {
		switch r.Decision {
		case DecisionDeny:
			decision = DecisionDeny
			if r.Reason != "" {
				reasons = append(reasons, r.Reason)
			}
		case DecisionAllow:
			if decision != DecisionDeny {
				decision = DecisionAllow
			}
		case DecisionNone:
			// No change.
		}
		if r.Halt {
			halt = true
			if r.Reason != "" && r.Decision != DecisionDeny {
				// A halting hook that didn't also deny still contributes
				// its reason so the user sees it.
				reasons = append(reasons, r.Reason)
			}
		}
		if r.Context != "" {
			contexts = append(contexts, r.Context)
		}
		if r.SystemMessage != "" {
			systems = append(systems, r.SystemMessage)
		}
		if r.UpdatedInput != "" {
			next, err := shallowMerge(merged, r.UpdatedInput)
			if err != nil {
				slog.Warn(
					"Hook updated_input patch rejected; ignoring",
					"error", err,
					"patch", r.UpdatedInput,
				)
				continue
			}
			merged = next
			anyPatch = true
		}
	}

	agg := AggregateResult{
		Decision:  decision,
		Halt:      halt,
		HookCount: len(results),
	}
	if anyPatch {
		agg.UpdatedInput = merged
	}
	if len(reasons) > 0 {
		agg.Reason = strings.Join(reasons, "\n")
	}
	if len(contexts) > 0 {
		agg.Context = strings.Join(contexts, "\n")
	}
	if len(systems) > 0 {
		agg.SystemMessage = strings.Join(systems, "\n")
	}
	return agg
}

// shallowMerge applies a top-level-keys patch to base (both JSON
// objects). Keys in patch overwrite keys in base; keys absent from the
// patch are preserved. Returns an error if either value is not a valid
// JSON object.
func shallowMerge(base, patch string) (string, error) {
	if base == "" {
		base = "{}"
	}
	// Ensure base is an object so sjson has somewhere to write.
	var baseAny any
	if err := json.Unmarshal([]byte(base), &baseAny); err != nil {
		return "", err
	}
	if _, ok := baseAny.(map[string]any); !ok {
		return "", errNotObject("tool_input")
	}
	var patchMap map[string]json.RawMessage
	if err := json.Unmarshal([]byte(patch), &patchMap); err != nil {
		return "", errNotObject("updated_input")
	}
	out := base
	for k, v := range patchMap {
		next, err := sjson.SetRawBytes([]byte(out), k, v)
		if err != nil {
			return "", err
		}
		out = string(next)
	}
	return out, nil
}

type errNotObject string

func (e errNotObject) Error() string { return string(e) + " is not a JSON object" }

// BlockedError is returned to a caller when hooks refused an action that
// has no tool result to carry the refusal: a blocked prompt, a halted
// compaction. Reason is what the hook wrote to stderr or to "reason".
type BlockedError struct {
	Event  string
	Reason string
}

func (e *BlockedError) Error() string {
	if e.Reason == "" {
		return e.Event + " hook blocked the action"
	}
	return e.Event + " hook: " + e.Reason
}

// MaxStopContinuations bounds how many times a Stop or SubagentStop hook
// may send the agent back to work in one turn. Hooks see stop_hook_active
// true on every continuation and are expected to let it stop; the cap is
// for the ones that do not.
const MaxStopContinuations = 5
