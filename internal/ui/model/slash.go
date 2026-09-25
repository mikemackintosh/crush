package model

import (
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/ui/util"
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
	case "login":
		// /login reopens sign-in for the current model's provider, or for
		// the named one, without waiting for a 401 to force it.
		providerID := arg
		if providerID == "" {
			providerID = m.currentProviderID()
		}
		if providerID == "" {
			return util.ReportError(errors.New("no provider selected; pick a model first or use /login <provider>")), true
		}
		if _, ok := m.com.Config().Providers.Get(providerID); !ok {
			return util.ReportError(fmt.Errorf("unknown provider %q", providerID)), true
		}
		return m.handleReAuthenticate(providerID), true
	}
	return nil, false
}

// currentProviderID is the provider behind the coder agent's model.
func (m *UI) currentProviderID() string {
	cfg := m.com.Config()
	if cfg == nil {
		return ""
	}
	agentCfg, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return ""
	}
	return cfg.Models[agentCfg.Model].Provider
}
