package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/opencode-ai/opencode/internal/config"
	"github.com/opencode-ai/opencode/internal/db"
	"github.com/opencode-ai/opencode/internal/llm/models"
	"github.com/opencode-ai/opencode/internal/llm/provider"
	"github.com/opencode-ai/opencode/internal/llm/tools"
	"github.com/opencode-ai/opencode/internal/message"
	"github.com/opencode-ai/opencode/internal/pubsub"
	"github.com/opencode-ai/opencode/internal/session"
	"github.com/stretchr/testify/require"
)

type blockingProvider struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingProvider) Model() models.Model { return models.Model{ID: models.GPT4o} }
func (p *blockingProvider) SendMessages(context.Context, []message.Message, []tools.BaseTool) (*provider.ProviderResponse, error) {
	return &provider.ProviderResponse{}, nil
}
func (p *blockingProvider) StreamResponse(ctx context.Context, _ []message.Message, _ []tools.BaseTool) <-chan provider.ProviderEvent {
	out := make(chan provider.ProviderEvent, 1)
	p.once.Do(func() { close(p.entered) })
	go func() {
		<-p.release
		out <- provider.ProviderEvent{Type: provider.EventError, Error: ctx.Err()}
		close(out)
	}()
	return out
}
func TestCancelRetainsSessionLockUntilExecutorStops(t *testing.T) {
	_, e := config.Load(t.TempDir(), false)
	require.NoError(t, e)
	conn, e := db.Open(t.TempDir())
	require.NoError(t, e)
	defer conn.Close()
	sessions := session.NewService(db.New(conn))
	sess, e := sessions.Create(context.Background(), "test")
	require.NoError(t, e)
	p := &blockingProvider{entered: make(chan struct{}), release: make(chan struct{})}
	locks := &sync.Map{}
	a := &agent{Broker: pubsub.NewBroker[AgentEvent](), provider: p, sessions: sessions, messages: message.NewService(db.New(conn)), activeRequests: locks}
	result, e := a.Run(context.Background(), sess.ID, "test")
	require.NoError(t, e)
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	a.Cancel(sess.ID)
	_, e = a.Run(context.Background(), sess.ID, "overlap")
	require.ErrorIs(t, e, ErrSessionBusy)
	close(p.release)
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("run did not stop")
	}
	require.Eventually(t, func() bool { return !a.IsSessionBusy(sess.ID) }, time.Second, time.Millisecond)
}
