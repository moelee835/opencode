package teams

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opencode-ai/opencode/internal/logging"
)

// Runner must honor cancellation and return only after execution has stopped.
// Busy includes user-initiated turns and summarization for the member session.
type Runner interface {
	Busy(string) bool
	Run(context.Context, Member, string) (string, error)
}
type Runtime struct {
	store  *Store
	runner Runner
	owner  string
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	active map[string]context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

func NewRuntime(ctx context.Context, store *Store, runner Runner) *Runtime {
	ctx, cancel := context.WithCancel(ctx)
	return &Runtime{store: store, runner: runner, owner: uuid.NewString(), ctx: ctx, cancel: cancel, active: map[string]context.CancelFunc{}}
}
func (r *Runtime) Start(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime closed")
	}
	if _, ok := r.active[id]; ok {
		return nil
	}
	epoch, e := r.store.Acquire(ctx, id, r.owner)
	if e != nil {
		return e
	}
	child, cancel := context.WithCancel(r.ctx)
	r.active[id] = cancel
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() { r.mu.Lock(); delete(r.active, id); r.mu.Unlock() }()
		r.loop(child, id, epoch)
	}()
	return nil
}
func (r *Runtime) Close() { r.mu.Lock(); r.closed = true; r.cancel(); r.mu.Unlock(); r.wg.Wait() }
func (r *Runtime) loop(ctx context.Context, id string, epoch int64) {
	type executing struct {
		cancel context.CancelFunc
		task   string
	}
	active := map[string]executing{}
	type finished struct {
		session string
		err     error
	}
	done := make(chan finished, 32)
	var workers sync.WaitGroup
	defer func() {
		for _, a := range active {
			a.cancel()
		}
		workers.Wait()
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.store.Release(releaseCtx, id, r.owner, epoch)
	}()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	heartbeat := time.NewTicker(3 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if e := r.store.Heartbeat(ctx, id, r.owner, epoch); e != nil {
				return
			}
		case result := <-done:
			delete(active, result.session)
			if result.err != nil {
				logging.Error("Team completion could not be committed; runtime stopped for recovery", "team", id, "error", result.err)
				return
			}
		case <-tick.C:
			var snapshot Snapshot
			var e error
			snapshot.Team, e = r.store.Get(ctx, id)
			if e == nil {
				snapshot.Members, e = r.store.Members(ctx, id)
			}
			if e == nil {
				snapshot.Tasks, e = r.store.Tasks(ctx, id)
			}
			if e != nil {
				continue
			}
			taskStatus := map[string]string{}
			for _, t := range snapshot.Tasks {
				taskStatus[t.ID] = t.Status
			}
			for _, m := range snapshot.Members {
				if running, ok := active[m.SessionID]; ok {
					if snapshot.Team.Status != "active" || m.Status != "running" || (running.task != "" && taskStatus[running.task] != "running") {
						running.cancel()
					}
					continue
				}
				if len(active) >= 8 || r.runner.Busy(m.SessionID) {
					continue
				}
				run, e := r.store.Claim(ctx, id, r.owner, epoch, m.SessionID)
				if errors.Is(e, sql.ErrNoRows) {
					continue
				}
				if e != nil {
					if errors.Is(e, ErrFenced) {
						return
					}
					continue
				}
				runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
				active[m.SessionID] = executing{cancel: cancel, task: run.TaskID}
				workers.Add(1)
				go func(m Member, run Run) {
					defer workers.Done()
					defer cancel()
					var result string
					var err error
					func() {
						defer func() {
							if p := recover(); p != nil {
								err = fmt.Errorf("runner panic: %v", p)
							}
						}()
						result, err = r.runner.Run(runCtx, m, run.Input)
					}()
					finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer finishCancel()
					// A fenced runtime cannot commit late results. The next owner recovers the run.
					finishErr := r.store.Finish(finishCtx, r.owner, run, result, err)
					select {
					case done <- finished{m.SessionID, finishErr}:
					case <-ctx.Done():
					}
				}(m, run)
			}
		}
	}
}
