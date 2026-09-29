// Package sessionfork copies a conversation into a new session so an
// alternative can be explored without disturbing the original. The fork
// is a full, independent session: its messages are fresh rows that carry
// the same parts, and the original is never touched.
package sessionfork

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// Sessions is the slice of session.Service a fork needs.
type Sessions interface {
	Get(ctx context.Context, id string) (session.Session, error)
	Create(ctx context.Context, title string) (session.Session, error)
	Save(ctx context.Context, s session.Session) (session.Session, error)
}

// Messages is the slice of message.Service a fork needs.
type Messages interface {
	List(ctx context.Context, sessionID string) ([]message.Message, error)
	Create(ctx context.Context, sessionID string, params message.CreateMessageParams) (message.Message, error)
}

// Options shape a fork.
type Options struct {
	// Title for the new session. Empty derives one from the source.
	Title string
	// UntilMessageID stops copying after this message, so the fork picks up
	// from a point in the past. Empty copies the whole conversation. The
	// value may be a prefix of the message id, as long as it is unique.
	UntilMessageID string
}

// ErrMessageNotFound is returned when UntilMessageID matches no message.
var ErrMessageNotFound = errors.New("no message with that id in the session")

// ErrAmbiguousMessage is returned when UntilMessageID matches several.
var ErrAmbiguousMessage = errors.New("message id prefix matches more than one message")

// Fork creates the new session and copies messages into it. Usage tokens
// carry over, because the copied context is what the next turn will be
// billed against; cost does not, because it was paid once. A compacted
// source keeps its summary boundary, remapped to the copied summary
// message, so the fork sends the model the same window the original
// would.
func Fork(ctx context.Context, sessions Sessions, messages Messages, sourceID string, opts Options) (session.Session, error) {
	source, err := sessions.Get(ctx, sourceID)
	if err != nil {
		return session.Session{}, fmt.Errorf("fork: source session: %w", err)
	}
	msgs, err := messages.List(ctx, sourceID)
	if err != nil {
		return session.Session{}, fmt.Errorf("fork: list messages: %w", err)
	}

	cut := len(msgs)
	if opts.UntilMessageID != "" {
		cut, err = cutIndex(msgs, opts.UntilMessageID)
		if err != nil {
			return session.Session{}, err
		}
	}
	msgs = msgs[:cut]

	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = ForkTitle(source.Title)
	}

	forked, err := sessions.Create(ctx, title)
	if err != nil {
		return session.Session{}, fmt.Errorf("fork: create session: %w", err)
	}

	idMap := make(map[string]string, len(msgs))
	for i := range msgs {
		src := &msgs[i]
		copied, err := messages.Create(ctx, forked.ID, message.CreateMessageParams{
			Role:             src.Role,
			Parts:            partsForCopy(src),
			Model:            src.Model,
			Provider:         src.Provider,
			IsSummaryMessage: src.IsSummaryMessage,
		})
		if err != nil {
			return session.Session{}, fmt.Errorf("fork: copy message %s: %w", src.ID, err)
		}
		idMap[src.ID] = copied.ID
	}

	forked.PromptTokens = source.PromptTokens
	forked.CompletionTokens = source.CompletionTokens
	forked.EstimatedUsage = source.EstimatedUsage
	forked.MessageCount = int64(len(msgs))
	if summary, ok := idMap[source.SummaryMessageID]; ok {
		forked.SummaryMessageID = summary
	}
	saved, err := sessions.Save(ctx, forked)
	if err != nil {
		return session.Session{}, fmt.Errorf("fork: save session: %w", err)
	}
	return saved, nil
}

// partsForCopy returns the parts to hand Create. The store appends its own
// Finish part to every non-assistant message it creates, so copying the
// stored one as well would leave the fork with two; the assistant's is
// kept because it carries the real finish reason.
func partsForCopy(m *message.Message) []message.ContentPart {
	if m.Role == message.Assistant {
		return m.Parts
	}
	out := make([]message.ContentPart, 0, len(m.Parts))
	for _, p := range m.Parts {
		if _, isFinish := p.(message.Finish); isFinish {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ForkTitle derives the new session's title from the source's.
func ForkTitle(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "Fork"
	}
	return "Fork of " + source
}

// cutIndex returns the number of messages to keep so the last kept one is
// the message identified by id or a unique prefix of it.
func cutIndex(msgs []message.Message, id string) (int, error) {
	found := -1
	for i := range msgs {
		if msgs[i].ID == id {
			return i + 1, nil
		}
		if strings.HasPrefix(msgs[i].ID, id) {
			if found >= 0 {
				return 0, ErrAmbiguousMessage
			}
			found = i
		}
	}
	if found < 0 {
		return 0, ErrMessageNotFound
	}
	return found + 1, nil
}
