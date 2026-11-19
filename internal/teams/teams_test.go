package teams

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opencode-ai/opencode/internal/db"
	"github.com/opencode-ai/opencode/internal/messaging"
	"github.com/opencode-ai/opencode/internal/session"
	"github.com/stretchr/testify/require"
)

func fixture(t *testing.T) (*Store, Team) {
	t.Helper()
	conn, e := db.Open(t.TempDir())
	require.NoError(t, e)
	t.Cleanup(func() { conn.Close() })
	lead, e := session.NewService(db.New(conn)).Create(context.Background(), "lead")
	require.NoError(t, e)
	s := NewStore(conn)
	team, e := s.Create(context.Background(), "test-team", lead.ID)
	require.NoError(t, e)
	return s, team
}
func worker(t *testing.T, s *Store, team Team, name string) Member {
	t.Helper()
	m, e := s.AddMember(context.Background(), team.ID, team.Lead, name, "Review assigned files and report", 30)
	require.NoError(t, e)
	return m
}
func TestTaskDependenciesAndAtomicClaims(t *testing.T) {
	s, team := fixture(t)
	ctx := context.Background()
	a := worker(t, s, team, "a")
	b := worker(t, s, team, "b")
	// Remove the initial tasks to isolate explicit shared work.
	_, e := s.Conn.Exec("DELETE FROM team_tasks")
	require.NoError(t, e)
	first, e := s.AddTask(ctx, team.ID, team.Lead, "first", "work", "", nil, false)
	require.NoError(t, e)
	second, e := s.AddTask(ctx, team.ID, team.Lead, "second", "work", "", []string{first.ID}, false)
	require.NoError(t, e)
	epoch, e := s.Acquire(ctx, team.ID, "runtime")
	require.NoError(t, e)
	var dbSeq int
	var dbName, dbPath string
	require.NoError(t, s.Conn.QueryRow("PRAGMA database_list").Scan(&dbSeq, &dbName, &dbPath))
	otherConn, e := db.Open(filepath.Dir(dbPath))
	require.NoError(t, e)
	defer otherConn.Close()
	other := NewStore(otherConn)
	results := make(chan Run, 2)
	errs := make(chan error, 2)
	for _, m := range []Member{a, b} {
		go func(m Member) {
			store := s
			if m.SessionID == b.SessionID {
				store = other
			}
			r, e := store.Claim(ctx, team.ID, "runtime", epoch, m.SessionID)
			results <- r
			errs <- e
		}(m)
	}
	var chosen Run
	count := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.ID != "" {
			chosen = r
			count++
		}
	}
	for i := 0; i < 2; i++ {
		e := <-errs
		require.True(t, e == nil || errors.Is(e, sql.ErrNoRows), e)
	}
	require.Equal(t, 1, count)
	require.Equal(t, first.ID, chosen.TaskID)
	require.NoError(t, s.Finish(ctx, "runtime", chosen, "done", nil))
	next, e := s.Claim(ctx, team.ID, "runtime", epoch, chosen.SessionID)
	require.NoError(t, e)
	require.Equal(t, second.ID, next.TaskID)
	snapshot, e := s.Snapshot(ctx, team.ID)
	require.NoError(t, e)
	require.Len(t, snapshot.Tasks, 2)
}
func TestAuthorizationApprovalAndLifecycle(t *testing.T) {
	s, team := fixture(t)
	ctx := context.Background()
	m := worker(t, s, team, "a")
	_, e := s.AddMember(ctx, team.ID, m.SessionID, "bad", "work", 30)
	require.Error(t, e)
	require.Error(t, s.Control(ctx, team.ID, m.SessionID, "", "stop"))
	_, e = s.AddTask(ctx, team.ID, team.Lead, "bad", "work", "", []string{"missing"}, false)
	require.Error(t, e)
	task, e := s.AddTask(ctx, team.ID, team.Lead, "reviewed", "work", m.SessionID, nil, true)
	require.NoError(t, e)
	require.Error(t, s.Assign(ctx, team.ID, team.Lead, task.ID, team.Lead))
	require.NoError(t, s.Assign(ctx, team.ID, team.Lead, task.ID, m.SessionID))
	require.NoError(t, s.Control(ctx, team.ID, team.Lead, task.ID, "approve"))
	require.Error(t, s.Control(ctx, team.ID, team.Lead, task.ID, "approve"))
	require.NoError(t, s.Control(ctx, team.ID, team.Lead, m.SessionID, "pause"))
	epoch, e := s.Acquire(ctx, team.ID, "r")
	require.NoError(t, e)
	_, e = s.Claim(ctx, team.ID, "r", epoch, m.SessionID)
	require.ErrorIs(t, e, sql.ErrNoRows)
	require.NoError(t, s.Control(ctx, team.ID, team.Lead, m.SessionID, "resume"))
	r, e := s.Claim(ctx, team.ID, "r", epoch, m.SessionID)
	require.NoError(t, e)
	require.NoError(t, s.Control(ctx, team.ID, team.Lead, r.TaskID, "cancel"))
	require.NoError(t, s.Finish(ctx, "r", r, "", context.Canceled))
	tasks, e := s.Tasks(ctx, team.ID)
	require.NoError(t, e)
	for _, task := range tasks {
		if task.ID == r.TaskID {
			require.Equal(t, "canceled", task.Status)
		}
	}
}
func TestLeaseFencingRecoveryAndNoReplay(t *testing.T) {
	s, team := fixture(t)
	ctx := context.Background()
	m := worker(t, s, team, "a")
	epoch, e := s.Acquire(ctx, team.ID, "old")
	require.NoError(t, e)
	_, e = s.Acquire(ctx, team.ID, "other")
	require.ErrorIs(t, e, ErrOwned)
	run, e := s.Claim(ctx, team.ID, "old", epoch, m.SessionID)
	require.NoError(t, e)
	_, e = s.Conn.Exec("UPDATE teams SET lease_until=0 WHERE id=?", team.ID)
	require.NoError(t, e)
	next, e := s.Acquire(ctx, team.ID, "new")
	require.NoError(t, e)
	require.Greater(t, next, epoch)
	require.ErrorIs(t, s.Finish(ctx, "old", run, "late result", nil), ErrFenced)
	require.ErrorIs(t, s.Heartbeat(ctx, team.ID, "old", epoch), ErrFenced)
	snapshot, e := s.Snapshot(ctx, team.ID)
	require.NoError(t, e)
	require.Equal(t, "failed", snapshot.Runs[0].Status)
	_, e = s.Claim(ctx, team.ID, "new", next, m.SessionID)
	require.ErrorIs(t, e, sql.ErrNoRows)
	require.NoError(t, s.Control(ctx, team.ID, team.Lead, m.SessionID, "resume"))
	require.NoError(t, s.Control(ctx, team.ID, team.Lead, run.TaskID, "retry"))
	_, e = s.Claim(ctx, team.ID, "new", next, m.SessionID)
	require.NoError(t, e)
}
func TestMailboxAutomaticDeliveryAndBudget(t *testing.T) {
	s, team := fixture(t)
	ctx := context.Background()
	m := worker(t, s, team, "a")
	_, e := s.Conn.Exec("DELETE FROM team_tasks")
	require.NoError(t, e)
	_, e = s.Conn.Exec("UPDATE team_members SET max_turns=1 WHERE session_id=?", m.SessionID)
	require.NoError(t, e)
	mail := messaging.New(s.Conn)
	sent, e := mail.Send(ctx, team.Lead, m.SessionID, "Please investigate", "one")
	require.NoError(t, e)
	epoch, e := s.Acquire(ctx, team.ID, "r")
	require.NoError(t, e)
	run, e := s.Claim(ctx, team.ID, "r", epoch, m.SessionID)
	require.NoError(t, e)
	require.Equal(t, sent.ID, run.MailID)
	require.NoError(t, s.Finish(ctx, "r", run, "found the issue", nil))
	require.Error(t, s.Finish(ctx, "r", run, "duplicate", nil))
	_, e = s.Claim(ctx, team.ID, "r", epoch, m.SessionID)
	require.ErrorIs(t, e, sql.ErrNoRows)
	leadRun, e := s.Claim(ctx, team.ID, "r", epoch, team.Lead)
	require.NoError(t, e)
	require.Contains(t, leadRun.Input, "found the issue")
	require.NoError(t, s.Finish(ctx, "r", leadRun, "summary", nil))
	all, e := mail.List(ctx, "", 0, false)
	require.NoError(t, e)
	require.Len(t, all, 2) // No reply-to-reply loop.
}

type fakeRunner struct {
	mu      sync.Mutex
	active  map[string]int
	seen    []string
	overlap bool
	block   bool
}

func (f *fakeRunner) Busy(string) bool { return false }
func (f *fakeRunner) Run(ctx context.Context, m Member, input string) (string, error) {
	f.mu.Lock()
	f.active[m.SessionID]++
	if f.active[m.SessionID] > 1 {
		f.overlap = true
	}
	f.seen = append(f.seen, m.SessionID)
	block := f.block
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active[m.SessionID]--; f.mu.Unlock() }()
	if block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return "completed by " + m.Name, nil
	}
}
func TestRuntimeAutomaticallyRunsWorkersAndLead(t *testing.T) {
	s, team := fixture(t)
	m := worker(t, s, team, "a")
	runner := &fakeRunner{active: map[string]int{}}
	runtime := NewRuntime(context.Background(), s, runner)
	defer runtime.Close()
	require.NoError(t, runtime.Start(context.Background(), team.ID))
	require.Eventually(t, func() bool { runner.mu.Lock(); defer runner.mu.Unlock(); return len(runner.seen) >= 2 }, 5*time.Second, 20*time.Millisecond)
	runner.mu.Lock()
	require.Contains(t, runner.seen, m.SessionID)
	require.Contains(t, runner.seen, team.Lead)
	require.False(t, runner.overlap)
	runner.mu.Unlock()
}
func TestRuntimeStopCancelsAndReleases(t *testing.T) {
	s, team := fixture(t)
	worker(t, s, team, "a")
	runner := &fakeRunner{active: map[string]int{}, block: true}
	runtime := NewRuntime(context.Background(), s, runner)
	require.NoError(t, runtime.Start(context.Background(), team.ID))
	require.Eventually(t, func() bool { runner.mu.Lock(); defer runner.mu.Unlock(); return len(runner.seen) > 0 }, 3*time.Second, 20*time.Millisecond)
	done := make(chan struct{})
	go func() { runtime.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not cancel")
	}
	tstate, e := s.Get(context.Background(), team.ID)
	require.NoError(t, e)
	require.Empty(t, tstate.Owner)
	snapshot, e := s.Snapshot(context.Background(), team.ID)
	require.NoError(t, e)
	require.Equal(t, "failed", snapshot.Runs[0].Status)
}

func TestMailQueuedDuringActiveTurn(t *testing.T) {
	s, team := fixture(t)
	ctx := context.Background()
	m := worker(t, s, team, "worker")
	epoch, e := s.Acquire(ctx, team.ID, "r")
	require.NoError(t, e)
	run, e := s.Claim(ctx, team.ID, "r", epoch, m.SessionID)
	require.NoError(t, e)
	require.Error(t, s.Assign(ctx, team.ID, team.Lead, run.TaskID, m.SessionID))
	sent, e := s.Tell(ctx, team.ID, m.Name, "Follow-up instruction")
	require.NoError(t, e)
	_, e = s.Claim(ctx, team.ID, "r", epoch, m.SessionID)
	require.ErrorIs(t, e, sql.ErrNoRows)
	require.NoError(t, s.Finish(ctx, "r", run, "completed initial task", nil))
	next, e := s.Claim(ctx, team.ID, "r", epoch, m.SessionID)
	require.NoError(t, e)
	require.Equal(t, sent.ID, next.MailID)
}
