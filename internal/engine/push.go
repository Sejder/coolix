package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"coolix/internal/gitpush"
	"coolix/internal/sshkeys"
)

// SSHKeys are the keys that may push (besides those declared in NixOS).
func (c *Config) SSHKeys() *sshkeys.Store { return sshkeys.Open(c.StateDir) }

// Push is one SSH git request, as sshd hands it to `coolix ssh-serve`.
type Push struct {
	Command gitpush.Command
	// Who pushed, for the log: the key's name or fingerprint.
	Who            string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// ServeGit answers a git fetch or push for a push app. A push to the app's
// deploy branch deploys it, with the output going to the pusher.
func (e *Engine) ServeGit(ctx context.Context, p Push) error {
	app, err := e.App(p.Command.App)
	if err != nil || !app.Pushed() || app.IsPreview() {
		return fmt.Errorf("there is no push app %q; create one with source \"push\" (coolix app add %s --source push)", p.Command.App, p.Command.App)
	}
	dir := gitpush.Dir(e.GitDir(), app.Name)
	if err := gitpush.Init(dir); err != nil {
		return err
	}
	run := func(service string) error {
		cmd := exec.CommandContext(ctx, "git", strings.TrimPrefix(service, "git-"), dir)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = p.Stdin, p.Stdout, p.Stderr
		return cmd.Run()
	}
	if p.Command.Service == "git-upload-pack" {
		return run(p.Command.Service)
	}

	before, err := gitpush.Branches(dir)
	if err != nil {
		return err
	}
	if err := run(p.Command.Service); err != nil {
		return err
	}
	after, err := gitpush.Branches(dir)
	if err != nil {
		return err
	}
	changes := gitpush.Changes(before, after)
	if len(changes) > 0 {
		e.logf("%s pushed to %s: %v", p.Who, app.Name, changes)
	}
	var errs []error
	for _, c := range changes {
		switch {
		case c.Rev == "":
			// A deleted branch takes its preview with it.
			if name, ok := e.FindPreview(app.Name, c.Branch); ok {
				e.logf("removing preview %s", name)
				errs = append(errs, e.RemovePreview(ctx, app.Name, name))
			}
			continue
		case gitpush.Pin(dir, c.Rev) != nil:
			return fmt.Errorf("keeping %s", c.Rev)
		case c.Branch == app.DeployBranch():
			e.logf("deploying %s (%s)", app.Name, ShortRev(c.Rev))
			errs = append(errs, e.Deploy(ctx, app.Name))
		case app.PreviewsEnabled():
			pv := Preview{Parent: app.Name, Branch: c.Branch}
			err := e.DeployPreview(ctx, pv)
			if err == nil {
				e.logf("preview of %s: %s.%s", c.Branch, pv.Name(), e.previewDomain(app))
			}
			errs = append(errs, err)
		default:
			e.logf("only pushes to %s deploy %s (pushes to other branches deploy previews if you turn them on)", app.DeployBranch(), app.Name)
		}
	}
	return errors.Join(errs...)
}

// initPushRepo creates a push app's bare repo, so it can be pushed to.
func (e *Engine) initPushRepo(app string) error {
	return gitpush.Init(gitpush.Dir(e.GitDir(), app))
}

// removePushRepo deletes a removed app's pushed code, so a new app of the
// same name starts empty.
func (e *Engine) removePushRepo(app string) error {
	return os.RemoveAll(gitpush.Dir(e.GitDir(), app))
}

// PushRemote is the git remote of a push app, for display: host is
// services.coolix.git.host.
func (c *Config) PushRemote(app string) string {
	if c.GitHost == "" {
		return ""
	}
	host := c.GitHost
	if slices.Contains([]string{"", "22"}, c.GitPort) {
		return "coolix@" + host + ":" + app
	}
	return "ssh://coolix@" + host + ":" + c.GitPort + "/" + app
}
