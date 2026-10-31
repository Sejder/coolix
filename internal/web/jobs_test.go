package web

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestJobStreaming(t *testing.T) {
	var js Jobs
	release := make(chan struct{})
	j := js.Start("Deploy blog", "blog", func(_ context.Context, j *Job) error {
		j.Write([]byte("line 1\nline "))
		<-release
		j.Write([]byte("2\n"))
		return errors.New("boom")
	})

	deadline := time.After(2 * time.Second)
	var got []string
	for {
		lines, done, err, next := j.Snapshot(len(got))
		got = append(got, lines...)
		if len(got) == 1 && release != nil {
			close(release)
			release = nil
		}
		if done {
			if err == nil || err.Error() != "boom" {
				t.Fatalf("err = %v", err)
			}
			break
		}
		select {
		case <-next:
		case <-deadline:
			t.Fatalf("timed out, got %q", got)
		}
	}
	want := []string{"line 1", "line 2", "error: boom"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	if j.State() != "failed" || js.Get(j.ID) != j || len(js.Recent(10, "other")) != 0 {
		t.Fatal("job bookkeeping wrong")
	}
}
