package permission

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestConcurrentRequestsAndCancellation(t *testing.T) {
	s := NewPermissionService()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := s.Subscribe(ctx)
	done := make(chan bool, 2)
	for _, id := range []string{"a", "b"} {
		go func(id string) {
			done <- s.Request(CreatePermissionRequest{Context: ctx, SessionID: id, Path: "/tmp/test", ToolName: "write"})
		}(id)
	}
	first := <-events
	second := <-events
	s.Grant(first.Payload)
	s.Grant(first.Payload)
	s.Deny(second.Payload)
	a, b := <-done, <-done
	require.NotEqual(t, a, b)
	go func() { done <- s.Request(CreatePermissionRequest{Context: ctx, SessionID: "c", Path: "/tmp/test"}) }()
	<-events
	cancel()
	select {
	case allowed := <-done:
		require.False(t, allowed)
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock permission")
	}
}
