package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/opencode-ai/opencode/internal/db"
	"github.com/opencode-ai/opencode/internal/messaging"
	"github.com/opencode-ai/opencode/internal/session"
	"github.com/opencode-ai/opencode/internal/teams"
	"github.com/stretchr/testify/require"
)

type starter struct{ ids []string }

func (s *starter) Start(_ context.Context, id string) error { s.ids = append(s.ids, id); return nil }
func TestTeamToolLifecycleAndMemberAuthorization(t *testing.T) {
	conn, e := db.Open(t.TempDir())
	require.NoError(t, e)
	defer conn.Close()
	lead, e := session.NewService(db.New(conn)).Create(context.Background(), "lead")
	require.NoError(t, e)
	store := teams.NewStore(conn)
	host := &starter{}
	permissions := &mailPermission{allow: true}
	tool := NewTeamTool(store, host, messaging.New(conn), permissions)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, lead.ID)
	invoke := func(ctx context.Context, p any) ToolResponse {
		data, e := json.Marshal(p)
		require.NoError(t, e)
		response, e := tool.Run(ctx, ToolCall{ID: "tool-call", Input: string(data)})
		require.NoError(t, e)
		return response
	}
	permissions.allow = false
	require.True(t, invoke(ctx, map[string]any{"action": "create", "name": "team"}).IsError)
	permissions.allow = true
	created := invoke(ctx, map[string]any{"action": "create", "name": "team"})
	require.False(t, created.IsError, created.Content)
	var team teams.Team
	require.NoError(t, json.Unmarshal([]byte(created.Content), &team))
	require.Equal(t, []string{team.ID}, host.ids)
	spawned := invoke(ctx, map[string]any{"action": "spawn", "name": "worker", "prompt": "Review code"})
	require.False(t, spawned.IsError, spawned.Content)
	var member teams.Member
	require.NoError(t, json.Unmarshal([]byte(spawned.Content), &member))
	workerCtx := context.WithValue(ctx, SessionIDContextKey, member.SessionID)
	require.True(t, invoke(workerCtx, map[string]any{"action": "spawn", "name": "forbidden", "prompt": "work"}).IsError)
	require.True(t, invoke(workerCtx, map[string]any{"action": "stop"}).IsError)
	require.False(t, invoke(workerCtx, map[string]any{"action": "task", "title": "Followup", "prompt": "Test changes", "approval": true}).IsError)
	require.False(t, invoke(ctx, map[string]any{"action": "message", "target": "all", "prompt": "Coordinate file ownership"}).IsError)
	mail, e := messaging.New(conn).List(ctx, member.SessionID, 0, false)
	require.NoError(t, e)
	require.Len(t, mail, 1)
	require.Contains(t, invoke(ctx, map[string]any{"action": "status"}).Content, "awaiting_approval")
	require.False(t, invoke(ctx, map[string]any{"action": "pause", "target": member.SessionID}).IsError)
	require.False(t, invoke(ctx, map[string]any{"action": "resume", "target": member.SessionID}).IsError)
}
