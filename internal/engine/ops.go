package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"coolix/internal/control"
	"coolix/internal/query"
	"coolix/internal/units"
	"coolix/internal/webhook"
)

// Webhooks ---------------------------------------------------------------

// Webhook tokens are secrets, so they live in the state dir (0600), not in
// the apps repo.
func (e *Engine) webhookPath(app string) string {
	return filepath.Join(e.StateDir, "webhooks", app+".token")
}

// WebhookToken returns the app's webhook token, or "" if webhooks are off.
func (e *Engine) WebhookToken(app string) (string, error) {
	if !units.ValidName(app) {
		return "", fmt.Errorf("invalid app name %q", app)
	}
	data, err := os.ReadFile(e.webhookPath(app))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}

// EnableWebhook creates (or rotates) the app's webhook token.
func (e *Engine) EnableWebhook(app string) (string, error) {
	if _, err := e.App(app); err != nil {
		return "", err
	}
	token, err := webhook.NewToken()
	if err != nil {
		return "", err
	}
	p := e.webhookPath(app)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(p+".tmp", []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, os.Rename(p+".tmp", p)
}

func (e *Engine) DisableWebhook(app string) error {
	if !units.ValidName(app) {
		return fmt.Errorf("invalid app name %q", app)
	}
	if err := os.Remove(e.webhookPath(app)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Backups ----------------------------------------------------------------

// Backup dumps the app's postgres database now (through coolix-control,
// which starts the root-rendered backup unit).
func (e *Engine) Backup(ctx context.Context, app string) error {
	if !units.ValidName(app) {
		return fmt.Errorf("invalid app name %q", app)
	}
	return e.privileged(ctx, control.Request{Verb: "backup", Arg: app})
}

// Restore replaces the app's database contents with a backup.
func (e *Engine) Restore(ctx context.Context, app, file string) error {
	if !units.ValidName(app) {
		return fmt.Errorf("invalid app name %q", app)
	}
	if !units.ValidBackupFile(file) {
		return fmt.Errorf("%q is not a backup name (see `coolix db backups %s`)", file, app)
	}
	files, err := e.Backups(ctx, app)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(files, func(f query.BackupFile) bool { return f.Name == file }) {
		return fmt.Errorf("%s has no backup %s", app, file)
	}
	return e.privileged(ctx, control.Request{Verb: "restore", Arg: app, File: file})
}

func (e *Engine) Backups(ctx context.Context, app string) ([]query.BackupFile, error) {
	if !units.ValidName(app) {
		return nil, fmt.Errorf("invalid app name %q", app)
	}
	return e.Query().Backups(ctx, app)
}

// Logs -------------------------------------------------------------------

// Logs writes the journal of an app (slots, databases, backups) to w.
func (e *Engine) Logs(ctx context.Context, app string, lines int, follow bool, w io.Writer) error {
	if !units.ValidName(app) {
		return fmt.Errorf("invalid app name %q", app)
	}
	return e.Query().Stream(ctx, query.Request{Kind: "logs", App: app, Lines: lines, Follow: follow}, w)
}
