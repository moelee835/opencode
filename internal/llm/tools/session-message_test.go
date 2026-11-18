package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/opencode-ai/opencode/internal/db"
	"github.com/opencode-ai/opencode/internal/messaging"
	"github.com/opencode-ai/opencode/internal/permission"
	"github.com/opencode-ai/opencode/internal/session"
	"github.com/stretchr/testify/require"
)

type mailPermission struct {
	permission.Service
	allow   bool
	request permission.CreatePermissionRequest
}

func (p *mailPermission) Request(r permission.CreatePermissionRequest) bool {
	p.request = r
	return p.allow
}

func TestSessionMessageTool(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	require.NoError(t, err)
	defer conn.Close()
	sessions := session.NewService(db.New(conn))
	ctx := context.Background()
	a, err := sessions.Create(ctx, "A")
	require.NoError(t, err)
	b, err := sessions.Create(ctx, "B")
	require.NoError(t, err)
	mail := messaging.New(conn)
	permissions := &mailPermission{}
	tool := NewSessionMessageTool(mail, sessions, permissions)
	ctx = context.WithValue(ctx, SessionIDContextKey, a.ID)
	call := func(ctx context.Context, input any) ToolResponse {
		data, err := json.Marshal(input)
		require.NoError(t, err)
		r, err := tool.Run(ctx, ToolCall{ID: "call-1", Input: string(data)})
		require.NoError(t, err)
		return r
	}
	send := map[string]any{"action": "send", "to": b.ID, "body": "hello", "from": b.ID}
	require.True(t, call(context.Background(), send).IsError)
	require.True(t, call(ctx, send).IsError)
	page, err := mail.List(ctx, b.ID, 0, false)
	require.NoError(t, err)
	require.Empty(t, page)
	permissions.allow = true
	r := call(ctx, send)
	require.False(t, r.IsError, r.Content)
	require.Equal(t, a.ID, permissions.request.SessionID)
	require.Equal(t, "send:"+b.ID, permissions.request.Action)
	require.False(t, call(ctx, send).IsError)
	page, err = mail.List(ctx, b.ID, 0, false)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, a.ID, page[0].SenderID)
	require.Equal(t, "[]", call(ctx, map[string]string{"action": "inbox"}).Content)
	require.True(t, call(ctx, map[string]string{"action": "read", "id": page[0].ID}).IsError)
	recipientCtx := context.WithValue(ctx, SessionIDContextKey, b.ID)
	require.Contains(t, call(recipientCtx, map[string]string{"action": "inbox"}).Content, "hello")
	require.False(t, call(recipientCtx, map[string]string{"action": "read", "id": page[0].ID}).IsError)
	require.Contains(t, call(ctx, map[string]string{"action": "sessions"}).Content, a.ID)
	require.True(t, call(ctx, map[string]string{"action": "invalid"}).IsError)
}
