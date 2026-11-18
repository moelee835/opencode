package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/opencode-ai/opencode/internal/db"
	"github.com/opencode-ai/opencode/internal/messaging"
	"github.com/opencode-ai/opencode/internal/session"
	"github.com/stretchr/testify/require"
)

func TestMessagesCLI(t *testing.T) {
	dir := t.TempDir()
	conn, err := db.Open(dir)
	require.NoError(t, err)
	defer conn.Close()
	sessions := session.NewService(db.New(conn))
	a, err := sessions.Create(context.Background(), "A")
	require.NoError(t, err)
	b, err := sessions.Create(context.Background(), "B")
	require.NoError(t, err)
	run := func(args ...string) (string, error) {
		cmd := newMessagesCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--data-dir", dir}, args...))
		err := cmd.Execute()
		return out.String(), err
	}
	text, err := run("sessions")
	require.NoError(t, err)
	require.Contains(t, text, a.ID)
	text, err = run("send", "--from", a.ID, "--to", b.ID, "--body", "hello", "--key", "cli-key")
	require.NoError(t, err)
	var m messaging.Message
	require.NoError(t, json.Unmarshal([]byte(text), &m))
	text, err = run("inbox", "--session", b.ID)
	require.NoError(t, err)
	require.Contains(t, text, m.ID)
	_, err = run("read", "--session", a.ID, "--id", m.ID)
	require.Error(t, err)
	_, err = run("read", "--session", b.ID, "--id", m.ID)
	require.NoError(t, err)
	text, err = run("inbox", "--session", b.ID, "--unread")
	require.NoError(t, err)
	require.Equal(t, "[]\n", text)
	_, err = run("send", "--from", a.ID)
	require.Error(t, err)
	_, err = run("inbox", "--session", "missing")
	require.Error(t, err)
}
