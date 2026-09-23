package shellconfig

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHookFromArgs(t *testing.T) {
	t.Parallel()
	h, err := HookFromArgs([]string{"--command", "./x.sh", "--matcher", "^bash$", "--timeout", "7", "--name", "x", "--async", "true"})
	require.NoError(t, err)
	require.Equal(t, "./x.sh", h["command"])
	require.Equal(t, "^bash$", h["matcher"])
	require.Equal(t, int64(7), h["timeout"])
	require.Equal(t, "x", h["name"])
	require.Equal(t, true, h["async"])

	_, err = HookFromArgs([]string{"--matcher", "x"})
	require.ErrorContains(t, err, "--command is required")

	_, err = HookFromArgs([]string{"--command", "x", "--bogus", "1"})
	require.ErrorContains(t, err, "unknown flag")
}
