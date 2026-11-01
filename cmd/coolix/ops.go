package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"coolix/internal/engine"
)

func runLogs(ctx context.Context, e *engine.Engine, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "follow")
	lines := fs.Int("n", 100, "number of lines")
	usage := errors.New("usage: coolix logs APP [-f] [-n LINES]")
	// Flags may come before or after the app name.
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return usage
	}
	app := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usage
	}
	return e.Logs(ctx, app, *lines, *follow, os.Stdout)
}

func runDB(ctx context.Context, e *engine.Engine, args []string) error {
	switch {
	case len(args) == 2 && args[0] == "backup":
		return e.Backup(ctx, args[1])
	case len(args) == 2 && args[0] == "backups":
		files, err := e.Backups(ctx, args[1])
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "BACKUP\tSIZE\tDATE")
		for _, f := range files {
			fmt.Fprintf(w, "%s\t%d\t%s\n", f.Name, f.Size, f.Time.Format("2006-01-02 15:04:05"))
		}
		return w.Flush()
	case len(args) == 3 && args[0] == "restore":
		return e.Restore(ctx, args[1], args[2])
	}
	return errors.New("usage: coolix db backup|backups APP, coolix db restore APP BACKUP")
}

func runWebhook(e *engine.Engine, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: coolix webhook enable|show|disable APP")
	}
	app := args[1]
	switch args[0] {
	case "enable":
		token, err := e.EnableWebhook(app)
		if err != nil {
			return err
		}
		printWebhook(app, token)
		return nil
	case "show":
		token, err := e.WebhookToken(app)
		if err != nil {
			return err
		}
		if token == "" {
			return fmt.Errorf("webhook for %s is off; run `coolix webhook enable %s`", app, app)
		}
		printWebhook(app, token)
		return nil
	case "disable":
		if err := e.DisableWebhook(app); err != nil {
			return err
		}
		logf("webhook for %s disabled", app)
		return nil
	}
	return fmt.Errorf("unknown webhook command %q", args[0])
}

func printWebhook(app, token string) {
	fmt.Printf("URL:    https://<ui domain>/hooks/%s\nSecret: %s\n", app, token)
	fmt.Println("\nUse the secret as the webhook secret (GitHub, Gitea, Forgejo), the secret token (GitLab)")
	fmt.Println("or as `Authorization: Bearer <secret>` (anything else). Pushes deploy the app.")
}

func runPreview(ctx context.Context, e *engine.Engine, args []string) error {
	switch {
	case len(args) == 3 && args[0] == "deploy":
		return e.DeployPreview(ctx, engine.Preview{Parent: args[1], Branch: args[2]})
	case len(args) == 2 && args[0] == "list":
		previews, err := e.Previews(args[1])
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "PREVIEW\tOF\tDOMAINS")
		for _, p := range previews {
			fmt.Fprintf(w, "%s\t%s\t%s\n", p.Name, p.PreviewRef, strings.Join(p.Domains, ","))
		}
		return w.Flush()
	case len(args) == 3 && args[0] == "rm":
		return e.RemovePreview(ctx, args[1], args[2])
	}
	return errors.New("usage: coolix preview deploy APP BRANCH, coolix preview list APP, coolix preview rm APP PREVIEW")
}
