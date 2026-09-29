package sessionfork

import (
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func services(t *testing.T) (session.Service, message.Service) {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	return session.NewService(db.New(conn), conn), message.NewService(db.New(conn))
}

func seed(t *testing.T, sessions session.Service, messages message.Service) (session.Session, []message.Message) {
	t.Helper()
	ctx := t.Context()
	src, err := sessions.Create(ctx, "Fix the build")
	require.NoError(t, err)
	var msgs []message.Message
	add := func(role message.MessageRole, parts ...message.ContentPart) {
		m, err := messages.Create(ctx, src.ID, message.CreateMessageParams{Role: role, Parts: parts, Model: "qwen", Provider: "llmgw"})
		require.NoError(t, err)
		msgs = append(msgs, m)
	}
	add(message.User, message.TextContent{Text: "make it green"})
	add(message.Assistant, message.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"go test"}`, Finished: true})
	add(message.Tool, message.ToolResult{ToolCallID: "c1", Name: "bash", Content: "ok"})
	add(message.Assistant, message.TextContent{Text: "done"})
	src.PromptTokens, src.CompletionTokens, src.Cost = 900, 120, 0.4
	src, err = sessions.Save(ctx, src)
	require.NoError(t, err)
	return src, msgs
}

func TestFork_CopiesEverythingIntoAnIndependentSession(t *testing.T) {
	sessions, messages := services(t)
	src, msgs := seed(t, sessions, messages)

	forked, err := Fork(t.Context(), sessions, messages, src.ID, Options{})
	require.NoError(t, err)
	require.NotEqual(t, src.ID, forked.ID)
	require.Equal(t, "Fork of Fix the build", forked.Title)
	require.Equal(t, int64(900), forked.PromptTokens, "usage carries over: the context is inherited")
	require.Equal(t, float64(0), forked.Cost, "cost does not: it was paid once")

	copied, err := messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Len(t, copied, len(msgs))
	for i := range msgs {
		require.NotEqual(t, msgs[i].ID, copied[i].ID, "fresh rows")
		require.Equal(t, msgs[i].Role, copied[i].Role)
		require.Equal(t, msgs[i].Parts, copied[i].Parts)
	}
	require.Equal(t, "go test", copied[1].ToolCalls()[0].Input[len(`{"command":"`):len(`{"command":"`)+7])

	original, err := messages.List(t.Context(), src.ID)
	require.NoError(t, err)
	require.Len(t, original, len(msgs), "the source is untouched")
}

func TestFork_UntilStopsAtAMessageAndAcceptsAPrefix(t *testing.T) {
	sessions, messages := services(t)
	src, msgs := seed(t, sessions, messages)

	forked, err := Fork(t.Context(), sessions, messages, src.ID, Options{Title: "Alt", UntilMessageID: msgs[1].ID[:8]})
	require.NoError(t, err)
	require.Equal(t, "Alt", forked.Title)
	copied, err := messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Len(t, copied, 2, "the user turn and the assistant's tool call, nothing after")
	require.Equal(t, int64(2), forked.MessageCount)

	_, err = Fork(t.Context(), sessions, messages, src.ID, Options{UntilMessageID: "nope"})
	require.ErrorIs(t, err, ErrMessageNotFound)
}

func TestFork_KeepsTheSummaryBoundary(t *testing.T) {
	sessions, messages := services(t)
	src, _ := seed(t, sessions, messages)
	summary, err := messages.Create(t.Context(), src.ID, message.CreateMessageParams{Role: message.Assistant, IsSummaryMessage: true, Parts: []message.ContentPart{message.TextContent{Text: "so far: tests pass"}}})
	require.NoError(t, err)
	src, err = sessions.Get(t.Context(), src.ID)
	require.NoError(t, err)
	src.SummaryMessageID = summary.ID
	_, err = sessions.Save(t.Context(), src)
	require.NoError(t, err)

	forked, err := Fork(t.Context(), sessions, messages, src.ID, Options{})
	require.NoError(t, err)
	require.NotEmpty(t, forked.SummaryMessageID)
	require.NotEqual(t, summary.ID, forked.SummaryMessageID, "remapped to the copied summary")
	copied, err := messages.Get(t.Context(), forked.SummaryMessageID)
	require.NoError(t, err)
	require.True(t, copied.IsSummaryMessage)
	require.Equal(t, forked.ID, copied.SessionID)
}

func TestCutIndex(t *testing.T) {
	t.Parallel()
	msgs := []message.Message{{ID: "aaaa-1"}, {ID: "aaab-2"}, {ID: "bbbb-3"}}
	n, err := cutIndex(msgs, "bbbb-3")
	require.NoError(t, err)
	require.Equal(t, 3, n)
	n, err = cutIndex(msgs, "aaab")
	require.NoError(t, err)
	require.Equal(t, 2, n)
	_, err = cutIndex(msgs, "aaa")
	require.ErrorIs(t, err, ErrAmbiguousMessage)
	_, err = cutIndex(msgs, "zzz")
	require.ErrorIs(t, err, ErrMessageNotFound)
	require.Equal(t, "Fork", ForkTitle("  "))
	require.Equal(t, "Fork of X", ForkTitle("X"))
}

func TestFork_RecordsItsSource(t *testing.T) {
	sessions, messages := services(t)
	src, _ := seed(t, sessions, messages)
	forked, err := Fork(t.Context(), sessions, messages, src.ID, Options{})
	require.NoError(t, err)
	require.Equal(t, src.ID, forked.ForkedFrom)
	reloaded, err := sessions.Get(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Equal(t, src.ID, reloaded.ForkedFrom, "lineage is persisted")
	listed, err := sessions.List(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 2, "a fork is a top-level session, not hidden like a task session")
}
