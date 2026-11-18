package messaging_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/opencode-ai/opencode/internal/db"
	"github.com/opencode-ai/opencode/internal/messaging"
	"github.com/opencode-ai/opencode/internal/session"
	"github.com/stretchr/testify/require"
)

func fixture(t *testing.T) (string, *sql.DB, *messaging.Service, string, string) {
	t.Helper()
	dir := t.TempDir()
	conn, err := db.Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	sessions := session.NewService(db.New(conn))
	a, err := sessions.Create(context.Background(), "sender")
	require.NoError(t, err)
	b, err := sessions.Create(context.Background(), "recipient")
	require.NoError(t, err)
	return dir, conn, messaging.New(conn), a.ID, b.ID
}

func TestSendPersistenceAndRead(t *testing.T) {
	dir, _, mail, a, b := fixture(t)
	ctx := context.Background()
	m, err := mail.Send(ctx, a, b, "안녕하세요", "retry-key")
	require.NoError(t, err)
	duplicate, err := mail.Send(ctx, a, b, "안녕하세요", "retry-key")
	require.NoError(t, err)
	require.Equal(t, m, duplicate)
	_, err = mail.Send(ctx, a, b, "changed", "retry-key")
	require.ErrorContains(t, err, "different content")
	// A new connection simulates an offline recipient returning later.
	other, err := db.Open(dir)
	require.NoError(t, err)
	defer other.Close()
	receiver := messaging.New(other)
	page, err := receiver.List(ctx, b, 0, true)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, m.ID, page[0].ID)
	empty, err := receiver.List(ctx, a, 0, false)
	require.NoError(t, err)
	require.Empty(t, empty)
	require.ErrorIs(t, receiver.MarkRead(ctx, a, m.ID), sql.ErrNoRows)
	require.NoError(t, receiver.MarkRead(ctx, b, m.ID))
	require.NoError(t, receiver.MarkRead(ctx, b, m.ID))
	page, err = mail.List(ctx, b, 0, true)
	require.NoError(t, err)
	require.Empty(t, page)
	page, err = mail.List(ctx, b, 0, false)
	require.NoError(t, err)
	require.NotNil(t, page[0].ReadAt)
}

func TestValidationAndDeletion(t *testing.T) {
	_, conn, mail, a, b := fixture(t)
	ctx := context.Background()
	for _, body := range []string{"", " \n", strings.Repeat("x", messaging.MaxBodyBytes+1), string([]byte{0xff})} {
		_, err := mail.Send(ctx, a, b, body, "")
		require.Error(t, err)
	}
	_, err := mail.Send(ctx, a, a, "self", "")
	require.Error(t, err)
	_, err = mail.Send(ctx, a, "missing", "text", "")
	require.Error(t, err)
	_, err = mail.Send(ctx, "missing", b, "text", "")
	require.Error(t, err)
	_, err = mail.Send(ctx, a, b, "text", strings.Repeat("k", 257))
	require.Error(t, err)
	_, err = mail.List(ctx, b, -1, false)
	require.Error(t, err)
	_, err = mail.Send(ctx, a, b, "text", "")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "DELETE FROM sessions WHERE id=?", b)
	require.NoError(t, err)
	page, err := mail.List(ctx, "", 0, false)
	require.NoError(t, err)
	require.Empty(t, page)
}

func TestWatchPaginationResumeAndCancellation(t *testing.T) {
	_, _, mail, a, b := fixture(t)
	ctx := context.Background()
	for i := 0; i < 105; i++ {
		_, err := mail.Send(ctx, a, b, "text", "")
		require.NoError(t, err)
	}
	page, err := mail.List(ctx, b, 0, false)
	require.NoError(t, err)
	require.Len(t, page, 100)
	watchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var received []messaging.Message
	err = mail.Watch(watchCtx, b, page[98].Seq, func(m messaging.Message) error {
		received = append(received, m)
		if len(received) == 6 {
			cancel()
		}
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, received, 6)
	for i := 1; i < len(received); i++ {
		require.Greater(t, received[i].Seq, received[i-1].Seq)
	}
	sentinel := errors.New("consumer failed")
	require.ErrorIs(t, mail.Watch(ctx, b, 0, func(m messaging.Message) error { return sentinel }), sentinel)
	// Failed delivery is replayable, since the callback did not acknowledge it.
	again, err := mail.List(ctx, b, 0, true)
	require.NoError(t, err)
	require.Len(t, again, 100)
}

func TestCrossProcessDelivery(t *testing.T) {
	dir, _, mail, a, b := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	received := make(chan messaging.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- mail.Watch(ctx, b, 0, func(m messaging.Message) error { received <- m; cancel(); return nil })
	}()
	child := exec.Command(os.Args[0], "-test.run=^TestMailSenderProcess$")
	child.Env = append(os.Environ(), "OPENCODE_MAIL_TEST_DIR="+dir, "OPENCODE_MAIL_FROM="+a, "OPENCODE_MAIL_TO="+b)
	output, err := child.CombinedOutput()
	require.NoError(t, err, string(output))
	select {
	case m := <-received:
		require.Equal(t, "from another process", m.Body)
	case <-ctx.Done():
		t.Fatal("message not delivered before deadline")
	}
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestMailSenderProcess(t *testing.T) {
	dir := os.Getenv("OPENCODE_MAIL_TEST_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	conn, err := db.Open(dir)
	require.NoError(t, err)
	defer conn.Close()
	_, err = messaging.New(conn).Send(context.Background(), os.Getenv("OPENCODE_MAIL_FROM"), os.Getenv("OPENCODE_MAIL_TO"), "from another process", "process-key")
	require.NoError(t, err)
}

func TestConcurrentSenders(t *testing.T) {
	dir, _, mail, a, b := fixture(t)
	other, err := db.Open(dir)
	require.NoError(t, err)
	defer other.Close()
	second := messaging.New(other)
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		service := mail
		if i%2 == 0 {
			service = second
		}
		go func() {
			_, err := service.Send(context.Background(), a, b, "same request", "concurrent-key")
			results <- err
		}()
	}
	for i := 0; i < 20; i++ {
		require.NoError(t, <-results)
	}
	page, err := mail.List(context.Background(), b, 0, false)
	require.NoError(t, err)
	require.Len(t, page, 1)
}
