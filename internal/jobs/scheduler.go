package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"markemind/dials/internal/db"
	"markemind/dials/internal/platform"
)

// Job runs either every Every, or once a day at Hour:Minute UTC.
type Job struct {
	Name   string
	Every  time.Duration
	Hour   int
	Minute int
	Daily  bool
	Fn     func(ctx context.Context) error

	running atomic.Bool
	next    time.Time
}

type Scheduler struct {
	DB      *db.DB
	Clock   platform.Clock
	Log     *slog.Logger
	OnError func(job string, err error)

	mu   sync.Mutex
	jobs map[string]*Job
	wg   sync.WaitGroup
}

func (s *Scheduler) Add(j *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		s.jobs = map[string]*Job{}
	}
	j.next = s.nextRun(j, s.Clock.Now(), true)
	s.jobs[j.Name] = j
}

func (s *Scheduler) nextRun(j *Job, now time.Time, first bool) time.Time {
	if j.Daily {
		t := now.UTC()
		n := time.Date(t.Year(), t.Month(), t.Day(), j.Hour, j.Minute, 0, 0, time.UTC)
		if !n.After(t) {
			n = n.Add(24 * time.Hour)
		}
		return n
	}
	if first {
		return now.Add(j.Every / 4) // stagger first runs after boot
	}
	return now.Add(j.Every)
}

// Run blocks until ctx is cancelled, then waits for running jobs.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	now := s.Clock.Now()
	s.mu.Lock()
	var due []*Job
	for _, j := range s.jobs {
		if !now.Before(j.next) {
			j.next = s.nextRun(j, now, false)
			due = append(due, j)
		}
	}
	s.mu.Unlock()
	for _, j := range due {
		s.start(ctx, j)
	}
}

func (s *Scheduler) start(ctx context.Context, j *Job) bool {
	if !j.running.CompareAndSwap(false, true) {
		return false // never overlap
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer j.running.Store(false)
		s.execute(ctx, j)
	}()
	return true
}

func (s *Scheduler) execute(ctx context.Context, j *Job) {
	started := s.Clock.Now()
	s.DB.W.ExecContext(ctx, `INSERT INTO job_runs(name, started, finished, ok, msg) VALUES(?,?,0,0,'running')
		ON CONFLICT(name) DO UPDATE SET started=excluded.started, msg='running'`, j.Name, started.Unix())
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
			}
		}()
		return j.Fn(ctx)
	}()
	ok, msg := 1, fmt.Sprintf("ok in %s", s.Clock.Now().Sub(started).Round(time.Millisecond))
	if err != nil && !errors.Is(err, context.Canceled) {
		ok, msg = 0, err.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		s.Log.Error("job failed", "job", j.Name, "err", err)
		if s.OnError != nil {
			s.OnError(j.Name, err)
		}
	}
	s.DB.W.ExecContext(context.WithoutCancel(ctx), `UPDATE job_runs SET finished=?, ok=?, msg=? WHERE name=?`, s.Clock.Now().Unix(), ok, msg, j.Name)
}

// RunNow triggers a job immediately (admin button). Returns false if it is already running.
func (s *Scheduler) RunNow(ctx context.Context, name string) (bool, error) {
	s.mu.Lock()
	j, ok := s.jobs[name]
	s.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("unknown job %q", name)
	}
	return s.start(ctx, j), nil
}

type JobStatus struct {
	Name     string `json:"name"`
	Running  bool   `json:"running"`
	Next     int64  `json:"next"`
	Started  int64  `json:"started"`
	Finished int64  `json:"finished"`
	OK       bool   `json:"ok"`
	Msg      string `json:"msg"`
}

func (s *Scheduler) Status(ctx context.Context) []JobStatus {
	s.mu.Lock()
	var out []JobStatus
	for _, j := range s.jobs {
		out = append(out, JobStatus{Name: j.Name, Running: j.running.Load(), Next: j.next.Unix()})
	}
	s.mu.Unlock()
	for i := range out {
		var ok int
		s.DB.R.QueryRowContext(ctx, `SELECT started, finished, ok, msg FROM job_runs WHERE name=?`, out[i].Name).
			Scan(&out[i].Started, &out[i].Finished, &ok, &out[i].Msg)
		out[i].OK = ok == 1
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
