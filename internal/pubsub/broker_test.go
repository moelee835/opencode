package pubsub

import (
	"context"
	"sync"
	"testing"
)

func TestConcurrentPublishUnsubscribeAndShutdown(t *testing.T) {
	b := NewBroker[int]()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		b.Subscribe(ctx)
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.Publish(CreatedEvent, j)
			}
		}()
		go func() { defer wg.Done(); cancel() }()
	}
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() { defer wg.Done(); b.Shutdown() }()
	}
	wg.Wait()
}
