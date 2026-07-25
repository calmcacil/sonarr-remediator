package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
)

// Task is a function that runs periodically.
type Task struct {
	Name     string
	Interval time.Duration
	Fn       func(ctx context.Context) error
}

// Scheduler runs periodic tasks.
type Scheduler struct {
	tasks []Task
	wg    sync.WaitGroup
}

// New creates a Scheduler.
func New(tasks ...Task) *Scheduler {
	return &Scheduler{tasks: tasks}
}

// Run starts all tasks. Blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	for _, task := range s.tasks {
		s.wg.Add(1)
		go s.runTask(ctx, task)
	}
	s.wg.Wait()
}

func (s *Scheduler) runTask(ctx context.Context, task Task) {
	defer s.wg.Done()

	ticker := time.NewTicker(task.Interval)
	defer ticker.Stop()

	// Run immediately
	if err := task.Fn(ctx); err != nil {
		logging.Logger.Error("scheduled task failed", "task", task.Name, "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := task.Fn(ctx); err != nil {
				logging.Logger.Error("scheduled task failed", "task", task.Name, "error", err)
			}
		}
	}
}