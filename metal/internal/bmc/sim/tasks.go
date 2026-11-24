package sim

import (
	"strconv"
	"time"
)

// Tasks model what the specification calls an asynchronous operation: the
// request is answered 202 with a task monitor, the work happens afterwards,
// and the task's state is how a client learns the outcome. The effect of the
// operation lands when the task completes, not when the request returns — a
// client that reads back too early sees the old world, as on hardware.
//
// A fixture with a zero task duration completes its tasks inside the request:
// the monitor still exists and reports Completed, but nothing is deferred.

type task struct {
	id      string
	name    string
	state   string // New | Running | Completed | Exception
	message string // why an Exception happened
	started time.Time
	ended   time.Time
}

// startTask registers a task for work and schedules it. work runs under the
// state lock when the task's duration has passed; its error turns the task
// into an Exception. Caller holds the lock.
func (s *State) startTask(name string, work func() error) *task {
	t := &task{id: strconv.Itoa(len(s.tasks) + 1), name: name, state: "New", started: time.Now()}
	s.tasks = append(s.tasks, t)
	d := s.f.Protocol.TaskDuration
	if d <= 0 {
		s.finishTask(t, work)
		return t
	}
	go func() {
		// Half the duration as New→Running, the rest as the work itself — two
		// edges a polling client can observe.
		time.Sleep(d / 2)
		s.mu.Lock()
		t.state = "Running"
		s.mu.Unlock()
		time.Sleep(d - d/2)
		s.mu.Lock()
		s.finishTask(t, work)
		s.mu.Unlock()
	}()
	return t
}

// finishTask runs the work and records the outcome. Caller holds the lock.
func (s *State) finishTask(t *task, work func() error) {
	t.state = "Completed"
	if err := work(); err != nil {
		t.state, t.message = "Exception", err.Error()
	}
	t.ended = time.Now()
}

// runTask is startTask for work that takes the state lock itself: the work
// runs outside the lock once the task's duration has passed, and its error
// turns the task into an Exception.
func (s *State) runTask(name string, work func() error) task {
	s.mu.Lock()
	t := &task{id: strconv.Itoa(len(s.tasks) + 1), name: name, state: "Running", started: time.Now()}
	s.tasks = append(s.tasks, t)
	d, view := s.f.Protocol.TaskDuration, *t
	s.mu.Unlock()
	go func() {
		time.Sleep(d)
		err := work()
		s.mu.Lock()
		defer s.mu.Unlock()
		t.state = "Completed"
		if err != nil {
			t.state, t.message = "Exception", err.Error()
		}
		t.ended = time.Now()
	}()
	return view
}

// tasksView clones the task list for the HTTP tier.
func (s *State) tasksView() []task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, *t)
	}
	return out
}

// noteTask records an operation that finished inside its request but is
// reported through a task monitor all the same — the iBMC answers its
// virtual-media action that way.
func (s *State) noteTask(name string) task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.startTask(name, func() error { return nil })
}
