package shell

import (
	"context"
	"testing"
	"time"

	"github.com/opencode-ai/opencode/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSessionsHaveIndependentShellState(t *testing.T) {
	_, e := config.Load(t.TempDir(), false)
	require.NoError(t, e)
	config.Get().Shell.Path = "/bin/sh"
	config.Get().Shell.Args = []string{}
	defer CloseAll()
	a := GetSessionShell("a", t.TempDir())
	b := GetSessionShell("b", t.TempDir())
	require.NotNil(t, a)
	require.NotNil(t, b)
	require.NotSame(t, a, b)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, code, _, e := a.Exec(ctx, "export OPENCODE_TEAM_MARKER=alpha", 5000)
	require.NoError(t, e)
	require.Zero(t, code)
	out, _, _, _, e := a.Exec(ctx, "printf '%s' \"$OPENCODE_TEAM_MARKER\"", 5000)
	require.NoError(t, e)
	require.Equal(t, "alpha", out)
	out, _, _, _, e = b.Exec(ctx, "printf '%s' \"${OPENCODE_TEAM_MARKER:-empty}\"", 5000)
	require.NoError(t, e)
	require.Equal(t, "empty", out)
	canceled, stop := context.WithCancel(context.Background())
	stop()
	_, _, _, _, e = a.Exec(canceled, "sleep 10", 10000)
	require.Error(t, e)
}
