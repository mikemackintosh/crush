package dialog

import (
	"testing"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func ids(list []session.Session) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = s.ID
	}
	return out
}

func TestGroupForks(t *testing.T) {
	t.Parallel()
	// Newest first, as ListSessions returns them.
	in := []session.Session{
		{ID: "f2", ForkedFrom: "a"},
		{ID: "b"},
		{ID: "f1a", ForkedFrom: "f1"},
		{ID: "f1", ForkedFrom: "a"},
		{ID: "a"},
		{ID: "orphan", ForkedFrom: "gone"},
	}
	require.Equal(t, []string{"b", "a", "f2", "f1", "f1a", "orphan"}, ids(groupForks(in)),
		"originals keep their order; forks follow their source, newest first, nested; an orphan is an original")

	require.Equal(t, []string{"x", "y"}, ids(groupForks([]session.Session{{ID: "x", ForkedFrom: "y"}, {ID: "y", ForkedFrom: "x"}})),
		"a cycle still lists every session once")
	require.Empty(t, groupForks(nil))
}

func TestSessionItemMarksForks(t *testing.T) {
	t.Parallel()
	plain := &SessionItem{Session: session.Session{Title: "Fix"}}
	fork := &SessionItem{Session: session.Session{Title: "Fix", ForkedFrom: "src"}}
	require.Equal(t, "Fix", plain.Filter())
	require.Equal(t, forkMarker+"Fix", fork.Filter())
}
