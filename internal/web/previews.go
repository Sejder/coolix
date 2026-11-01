package web

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"coolix/internal/appdef"
	"coolix/internal/engine"
	"coolix/internal/webhook"
)

// A preview request: deploy p, or remove the preview named name.
type previewReq struct {
	remove bool
	p      engine.Preview
	name   string
}

// previewJobs runs preview deploys and removals one at a time per preview.
// A request arriving while another job for the same preview waits replaces
// that job's request, so a burst of pushes to a PR deploys only the last.
type previewJobs struct {
	mu     sync.Mutex
	latest map[string]previewReq
	queued map[string]*Job
	locks  map[string]*sync.Mutex
}

func (s *Server) previewJob(title string, req previewReq) (*Job, bool) {
	pj := &s.previews
	pj.mu.Lock()
	defer pj.mu.Unlock()
	if pj.latest == nil {
		pj.latest, pj.queued, pj.locks = map[string]previewReq{}, map[string]*Job{}, map[string]*sync.Mutex{}
	}
	name := req.name
	pj.latest[name] = req
	if j := pj.queued[name]; j != nil {
		return j, false
	}
	lock := pj.locks[name]
	if lock == nil {
		lock = &sync.Mutex{}
		pj.locks[name] = lock
	}
	j := s.Jobs.Start(title, req.p.Parent, func(ctx context.Context, j *Job) error {
		lock.Lock()
		defer lock.Unlock()
		pj.mu.Lock()
		req := pj.latest[name]
		delete(pj.queued, name)
		pj.mu.Unlock()
		e := s.engine(j)
		if req.remove {
			return e.RemovePreview(ctx, req.p.Parent, name)
		}
		return e.DeployPreview(ctx, req.p)
	})
	pj.queued[name] = j
	return j, true
}

// handlePRHook turns pull request events into previews.
func (s *Server) handlePRHook(w http.ResponseWriter, app *appdef.App, d *webhook.Delivery) {
	pr := d.PR
	ignore := func(reason string) {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "{\"status\":\"ignored\",\"reason\":%q}\n", reason)
	}
	switch {
	case !app.PreviewsEnabled():
		ignore("previews are off for " + app.Name)
		return
	case pr.Fork:
		log.Printf("webhook: %s: ignoring pull request %d from a fork", app.Name, pr.Number)
		ignore("pull requests from forks are never deployed")
		return
	case pr.Action == "":
		ignore("pull request event " + d.Event)
		return
	}
	p := engine.Preview{Parent: app.Name, Number: pr.Number, Branch: pr.Branch, Rev: pr.Rev}
	req := previewReq{p: p, name: p.Name()}
	title := fmt.Sprintf("Preview %s of PR #%d (webhook from %s)", req.name, pr.Number, d.Provider)
	if pr.Action == "close" {
		name, ok := s.quiet().FindPreview(app.Name, p.Ref())
		if !ok {
			ignore("no preview of this pull request")
			return
		}
		req = previewReq{remove: true, p: p, name: name}
		title = fmt.Sprintf("Remove preview %s (PR #%d closed)", name, pr.Number)
	}
	j, started := s.previewJob(title, req)
	status := "deploying"
	if req.remove {
		status = "removing"
	}
	if !started {
		status = "queued"
	}
	log.Printf("webhook: %s pull request %d %s: %s %s as job %d", d.Provider, pr.Number, pr.Action, status, req.name, j.ID)
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "{\"status\":%q,\"preview\":%q,\"job\":%d}\n", status, req.name, j.ID)
}

func (s *Server) handleDeployPreview(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app, err := s.quiet().App(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	branch := strings.TrimSpace(r.FormValue("branch"))
	if !appdef.ValidBranch(branch) {
		s.redirect(w, r, "/apps/"+name, fmt.Sprintf("%q is not a branch name.", branch))
		return
	}
	p := engine.Preview{Parent: app.Name, Branch: branch}
	j, _ := s.previewJob("Preview "+p.Name()+" of "+branch, previewReq{p: p, name: p.Name()})
	s.redirect(w, r, j.URL(), "")
}

func (s *Server) handleRemovePreview(w http.ResponseWriter, r *http.Request) {
	parent, name := r.PathValue("name"), r.PathValue("preview")
	a, err := s.quiet().App(name)
	if err != nil || a.PreviewOf != parent {
		http.NotFound(w, r)
		return
	}
	j, _ := s.previewJob("Remove preview "+name, previewReq{remove: true, p: engine.Preview{Parent: parent}, name: name})
	s.redirect(w, r, j.URL(), "")
}
