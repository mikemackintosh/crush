package model

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
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
	case "model", "models":
		// /model opens the picker; ctrl+r inside it refetches the catalog.
		return m.openModelsDialog(), true
	case "sessions", "session":
		return m.openSessionsDialog(), true
	case "forks":
		// /forks jumps between a session and its most recent fork.
		return m.switchFork(), true
	case "fork":
		return m.forkCurrentSession(arg), true
	case "login":
		return m.reauthenticateCurrent(arg), true
	}
	return nil, false
}

// forkCurrentSession copies this conversation into a new session and
// switches to it, leaving the original where it is. Shared by the typed
// /fork and the palette's Fork Session.
func (m *UI) forkCurrentSession(title string) tea.Cmd {
	if !m.hasSession() {
		return util.ReportError(errors.New("nothing to fork yet; send a message first"))
	}
	sourceID := m.session.ID
	return func() tea.Msg {
		forked, err := m.com.Workspace.ForkSession(context.Background(), sourceID, title, "")
		if err != nil {
			return util.ReportError(err)()
		}
		return sessionForkedMsg{session: forked}
	}
}

// reauthenticateCurrent reopens sign-in for the current model's provider,
// or for the named one, without waiting for a 401 to force it.
func (m *UI) reauthenticateCurrent(providerID string) tea.Cmd {
	if providerID == "" {
		providerID = m.currentProviderID()
	}
	if providerID == "" {
		return util.ReportError(errors.New("no provider selected; pick a model first or use /login <provider>"))
	}
	if _, ok := m.com.Config().Providers.Get(providerID); !ok {
		return util.ReportError(fmt.Errorf("unknown provider %q", providerID))
	}
	return m.handleReAuthenticate(providerID)
}

// switchFork jumps from a fork back to its source, or from a source to its
// most recent fork. It is the quick toggle for comparing two branches of
// the same conversation without going through the sessions list.
func (m *UI) switchFork() tea.Cmd {
	if !m.hasSession() {
		return util.ReportInfo("no session to switch from")
	}
	current := *m.session
	return func() tea.Msg {
		sessions, err := m.com.Workspace.ListSessions(context.Background())
		if err != nil {
			return util.ReportError(err)()
		}
		target, ok := forkSwitchTarget(current, sessions)
		if !ok {
			return util.NewInfoMsg("This session has no fork yet; /fork makes one")
		}
		return sessionSwitchMsg{sessionID: target.ID, title: target.Title}
	}
}

// forkSwitchTarget picks where ctrl+shift+s goes: the source when current
// is a fork, otherwise the newest fork of current. Sessions are expected
// newest first, as ListSessions returns them.
func forkSwitchTarget(current session.Session, sessions []session.Session) (session.Session, bool) {
	if current.ForkedFrom != "" {
		for _, s := range sessions {
			if s.ID == current.ForkedFrom {
				return s, true
			}
		}
	}
	for _, s := range sessions {
		if s.ForkedFrom == current.ID {
			return s, true
		}
	}
	return session.Session{}, false
}

// sessionSwitchMsg asks the UI to load another session.
type sessionSwitchMsg struct{ sessionID, title string }

// sessionForkedMsg carries the new session after /fork.
type sessionForkedMsg struct{ session session.Session }

// describeModelCounts summarises the catalog for the refetch notice, so the
// user can see whether a gateway's models actually arrived.
func describeModelCounts(cfg *config.Config) string {
	if cfg == nil {
		return "no config"
	}
	providers, models := 0, 0
	for _, pc := range cfg.Providers.Seq2() {
		if pc.Disable {
			continue
		}
		providers++
		models += len(pc.Models)
	}
	return fmt.Sprintf("%d models across %d providers", models, providers)
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
