package model

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// parseSlashCommand recognises a one-line prompt of the form "/name [arg]".
// Only a single line qualifies: a multi-line prompt that happens to start with
// a slash is a prompt. name is lowercased; arg is the rest, trimmed.
func parseSlashCommand(content string) (name, arg string, ok bool) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "/") || strings.ContainsAny(trimmed, "\n\r") {
		return "", "", false
	}
	fields := strings.Fields(trimmed[1:])
	if len(fields) == 0 {
		return "", "", false
	}
	name = strings.ToLower(fields[0])
	for _, r := range name {
		if (r < 'a' || r > 'z') && r != '-' {
			return "", "", false
		}
	}
	return name, strings.TrimSpace(strings.TrimPrefix(trimmed[1:], fields[0])), true
}

// runSlashCommand executes a recognised slash command. handled reports whether
// the input was consumed; an unknown command is left to be sent as a prompt,
// so a user asking the model about "/etc" is not swallowed.
func (m *UI) runSlashCommand(name, arg string) (cmd tea.Cmd, handled bool) {
	switch name {
	case "mcp":
		// /mcp opens the MCP servers dialog; /mcp <name> opens it narrowed to
		// that server, ready for reconnect, enable/disable or sign-in.
		m.openMCPServersDialogFiltered(arg)
		return nil, true
	}
	return nil, false
}
