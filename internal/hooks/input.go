package hooks

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/charmbracelet/crush/internal/shell"
	"github.com/tidwall/gjson"
)

// SupportedOutputVersion is the highest envelope version this build
// understands. Hooks may omit `version` entirely (treated as 1) or pin
// an older version. Unknown higher versions are still parsed but logged.
const SupportedOutputVersion = 1

// Payload is the JSON structure piped to hook commands via stdin for the
// tool events. Both "event" (Crush) and "hook_event_name" (Claude Code)
// carry the event name. ToolInput is emitted as a parsed JSON object for
// compatibility with Claude Code hooks (which expect tool_input to be an
// object, not a string).
type Payload struct {
	Event         string          `json:"event"`
	HookEventName string          `json:"hook_event_name"`
	SessionID     string          `json:"session_id"`
	CWD           string          `json:"cwd"`
	ToolName      string          `json:"tool_name,omitempty"`
	ToolInput     json.RawMessage `json:"tool_input,omitempty"`
}

// BuildPayload constructs the JSON stdin payload for a tool-event hook.
func BuildPayload(eventName, sessionID, cwd, toolName, toolInputJSON string) []byte {
	return BuildEventPayload(Event{
		Name:      eventName,
		SessionID: sessionID,
		ToolName:  toolName,
		ToolInput: toolInputJSON,
	}, cwd)
}

// BuildEventPayload constructs the JSON stdin payload for any event. The
// fixed keys come first; ev.Fields are added verbatim, so an event can
// carry prompt, tool_response, stop_hook_active, trigger, source, reason
// or whatever else its hooks expect.
func BuildEventPayload(ev Event, cwd string) []byte {
	p := map[string]any{
		"event":           ev.Name,
		"hook_event_name": ev.Name,
		"session_id":      ev.SessionID,
		"cwd":             cwd,
	}
	if ev.ToolName != "" || ev.ToolInput != "" {
		toolInput := json.RawMessage(ev.ToolInput)
		if !json.Valid(toolInput) {
			toolInput = json.RawMessage("{}")
		}
		p["tool_name"] = ev.ToolName
		p["tool_input"] = toolInput
	}
	for k, v := range ev.Fields {
		p[k] = v
	}
	data, err := json.Marshal(p)
	if err != nil {
		return []byte("{}")
	}
	return data
}

// BuildEnv constructs the environment variable slice for a hook command.
// It includes all current process env vars plus hook-specific ones.
func BuildEnv(eventName, toolName, sessionID, cwd, projectDir, toolInputJSON string) []string {
	env := os.Environ()
	env = append(env, shell.CrushEnvMarkers()...)
	env = append(
		env,
		fmt.Sprintf("CRUSH_EVENT=%s", eventName),
		fmt.Sprintf("CRUSH_TOOL_NAME=%s", toolName),
		fmt.Sprintf("CRUSH_SESSION_ID=%s", sessionID),
		fmt.Sprintf("CRUSH_CWD=%s", cwd),
		fmt.Sprintf("CRUSH_PROJECT_DIR=%s", projectDir),
	)

	// Extract tool-specific env vars from the JSON input.
	if toolInputJSON != "" {
		if cmd := gjson.Get(toolInputJSON, "command"); cmd.Exists() {
			env = append(env, fmt.Sprintf("CRUSH_TOOL_INPUT_COMMAND=%s", cmd.String()))
		}
		if fp := gjson.Get(toolInputJSON, "file_path"); fp.Exists() {
			env = append(env, fmt.Sprintf("CRUSH_TOOL_INPUT_FILE_PATH=%s", fp.String()))
		}
	}

	return env
}

// parseStdout parses the JSON output from a hook command's stdout.
// Supports both Crush format and Claude Code format (hookSpecificOutput).
// Non-JSON output is ignored.
func parseStdout(stdout string) HookResult {
	return parseStdoutFor(stdout, false)
}

// parseStdoutFor parses hook stdout. When plainContext is set, output that
// is not a JSON object is returned as context for the model, which is what
// UserPromptSubmit and SessionStart hooks expect.
func parseStdoutFor(stdout string, plainContext bool) HookResult {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return HookResult{Decision: DecisionNone}
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		if plainContext {
			return HookResult{Decision: DecisionNone, Context: stdout}
		}
		return HookResult{Decision: DecisionNone}
	}

	var common struct {
		Version       int             `json:"version"`
		Decision      string          `json:"decision"`
		Halt          bool            `json:"halt"`
		Continue      *bool           `json:"continue"`
		StopReason    string          `json:"stopReason"`
		SystemMessage string          `json:"systemMessage"`
		Reason        string          `json:"reason"`
		Context       json.RawMessage `json:"context"`
		UpdatedInput  json.RawMessage `json:"updated_input"`
	}
	if err := json.Unmarshal([]byte(stdout), &common); err != nil {
		return HookResult{Decision: DecisionNone}
	}

	if common.Version > SupportedOutputVersion {
		slog.Debug(
			"Hook output declared a newer envelope version than this build supports",
			"version", common.Version,
			"supported", SupportedOutputVersion,
		)
	}

	result := HookResult{
		Halt:          common.Halt,
		Reason:        common.Reason,
		Context:       parseContext(common.Context),
		SystemMessage: common.SystemMessage,
		Decision:      parseDecision(common.Decision),
		UpdatedInput:  rawToString(common.UpdatedInput),
	}

	// Claude Code: "continue": false stops the agent after the hook, with
	// stopReason shown in place of the model's output.
	if common.Continue != nil && !*common.Continue {
		result.Halt = true
		result.Decision = DecisionDeny
		if result.Reason == "" {
			result.Reason = common.StopReason
		}
		if result.Reason == "" {
			result.Reason = "stopped by hook"
		}
	}

	// Claude Code: event-specific fields live under hookSpecificOutput and
	// refine the common ones.
	if hso, ok := raw["hookSpecificOutput"]; ok {
		specific := parseClaudeCodeOutput(hso)
		if specific.Decision != DecisionNone {
			result.Decision = specific.Decision
		}
		if specific.Reason != "" {
			result.Reason = specific.Reason
		}
		if specific.Context != "" {
			result.Context = joinContext(result.Context, specific.Context)
		}
		if specific.UpdatedInput != "" {
			result.UpdatedInput = specific.UpdatedInput
		}
	}
	return result
}

func joinContext(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n" + b
	}
}

// parseContext accepts either a single string or an array of strings and
// returns a newline-joined value with empty entries dropped.
func parseContext(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// String form.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	// Array form.
	if raw[0] == '[' {
		var items []string
		if err := json.Unmarshal(raw, &items); err != nil {
			return ""
		}
		out := items[:0]
		for _, s := range items {
			if s != "" {
				out = append(out, s)
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

// parseClaudeCodeOutput handles the Claude Code hookSpecificOutput object:
// {"permissionDecision": "allow", "updatedInput": {...}, "additionalContext": "..."}
func parseClaudeCodeOutput(data json.RawMessage) HookResult {
	var hso struct {
		PermissionDecision       string          `json:"permissionDecision"`
		PermissionDecisionReason string          `json:"permissionDecisionReason"`
		UpdatedInput             json.RawMessage `json:"updatedInput"`
		AdditionalContext        string          `json:"additionalContext"`
	}
	if err := json.Unmarshal(data, &hso); err != nil {
		return HookResult{Decision: DecisionNone}
	}

	result := HookResult{
		Decision: parseDecision(hso.PermissionDecision),
		Reason:   hso.PermissionDecisionReason,
		Context:  hso.AdditionalContext,
	}

	// Marshal updatedInput back to a string for our opaque format.
	if len(hso.UpdatedInput) > 0 && string(hso.UpdatedInput) != "null" {
		result.UpdatedInput = string(hso.UpdatedInput)
	}

	return result
}

// rawToString converts a json.RawMessage to a string suitable for use
// as opaque tool input. It accepts both a JSON object (nested) and a
// JSON string (stringified, for backward compatibility).
func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// If it's a JSON string, unwrap it.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	// Otherwise it's an object/array — use as-is.
	return string(raw)
}

// parseDecision maps the decision vocabulary of both formats: Crush's
// allow/deny and Claude Code's approve/block (and "ask", which means the
// normal permission prompt, i.e. no opinion).
func parseDecision(s string) Decision {
	switch strings.ToLower(s) {
	case "allow", "approve":
		return DecisionAllow
	case "deny", "block":
		return DecisionDeny
	default:
		return DecisionNone
	}
}
