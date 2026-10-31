package web

import (
	"bytes"
	"context"
	"strconv"
	"sync"
	"time"
)

// Job is a deploy or rollback running in the background. Its output is kept in
// memory and streamed to any number of viewers.
type Job struct {
	ID      int
	Title   string
	App     string
	Started time.Time

	mu       sync.Mutex
	lines    []string
	partial  []byte
	done     bool
	err      error
	finished time.Time
	notify   chan struct{} // closed and replaced on every update
}

func (j *Job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.partial = append(j.partial, p...)
	for {
		i := bytes.IndexByte(j.partial, '\n')
		if i < 0 {
			break
		}
		j.lines = append(j.lines, string(bytes.TrimRight(j.partial[:i], "\r")))
		j.partial = j.partial[i+1:]
	}
	j.wake()
	return len(p), nil
}

func (j *Job) wake() {
	close(j.notify)
	j.notify = make(chan struct{})
}

func (j *Job) finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.partial) > 0 {
		j.lines = append(j.lines, string(j.partial))
		j.partial = nil
	}
	j.done, j.err, j.finished = true, err, time.Now()
	j.wake()
}

// Snapshot returns lines from index from on, whether the job is done, its
// error, and a channel closed on the next update.
func (j *Job) Snapshot(from int) ([]string, bool, error, <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var lines []string
	if from < len(j.lines) {
		lines = append(lines, j.lines[from:]...)
	}
	return lines, j.done, j.err, j.notify
}

func (j *Job) State() string {
	_, done, err, _ := j.Snapshot(1 << 30)
	switch {
	case !done:
		return "running"
	case err != nil:
		return "failed"
	}
	return "succeeded"
}

func (j *Job) Duration() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	end := j.finished
	if !j.done {
		end = time.Now()
	}
	return end.Sub(j.Started).Round(time.Second)
}

func (j *Job) URL() string { return "/jobs/" + strconv.Itoa(j.ID) }

type Jobs struct {
	mu   sync.Mutex
	next int
	list []*Job // newest last
}

const keepJobs = 50

// Start runs fn in the background as a new job.
func (js *Jobs) Start(title, app string, fn func(ctx context.Context, j *Job) error) *Job {
	js.mu.Lock()
	js.next++
	j := &Job{ID: js.next, Title: title, App: app, Started: time.Now(), notify: make(chan struct{})}
	js.list = append(js.list, j)
	if len(js.list) > keepJobs {
		js.list = js.list[len(js.list)-keepJobs:]
	}
	js.mu.Unlock()
	go func() {
		err := fn(context.Background(), j)
		if err != nil {
			j.Write([]byte("error: " + err.Error() + "\n"))
		}
		j.finish(err)
	}()
	return j
}

func (js *Jobs) Get(id int) *Job {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, j := range js.list {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// Recent returns up to n jobs, newest first, optionally only for app.
func (js *Jobs) Recent(n int, app string) []*Job {
	js.mu.Lock()
	defer js.mu.Unlock()
	var out []*Job
	for i := len(js.list) - 1; i >= 0 && len(out) < n; i-- {
		if app == "" || js.list[i].App == app {
			out = append(out, js.list[i])
		}
	}
	return out
}
