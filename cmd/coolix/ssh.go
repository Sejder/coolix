package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"coolix/internal/engine"
	"coolix/internal/gitpush"
	"coolix/internal/sshkeys"
)

// sshServe is sshd's ForceCommand for the coolix user: it only answers git
// requests for push apps.
func sshServe(ctx context.Context, e *engine.Engine) error {
	keys, _ := e.SSHKeys().List()
	auth := ""
	if p := os.Getenv("SSH_USER_AUTH"); p != "" {
		data, _ := os.ReadFile(p)
		auth = string(data)
	}
	fp, name := sshkeys.Identify(auth, keys)
	who := name
	if who == "" {
		who = "key " + fp
	}
	orig := os.Getenv("SSH_ORIGINAL_COMMAND")
	if orig == "" {
		return fmt.Errorf("hi %s! coolix only accepts git push and fetch, e.g. git push coolix@host:APP main", who)
	}
	cmd, err := gitpush.ParseCommand(orig)
	if err != nil {
		return err
	}
	// Progress goes to the pusher's terminal; stdout carries the git protocol.
	e.Out = os.Stderr
	return e.ServeGit(ctx, engine.Push{Command: cmd, Who: who, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
}

// sshAuthorizedKeys is sshd's AuthorizedKeysCommand (as the coolix user):
// it prints the keys added in the UI. The state dir is its only argument,
// as sshd runs it without coolix's environment.
func sshAuthorizedKeys(_ context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: coolix ssh-authorized-keys STATE_DIR")
	}
	keys, err := sshkeys.Open(args[0]).List()
	if err != nil {
		return err
	}
	return sshkeys.WriteAuthorizedKeys(os.Stdout, keys)
}

func runSSHKey(e *engine.Engine, args []string) error {
	s := e.SSHKeys()
	switch {
	case len(args) >= 1 && len(args) <= 2 && args[0] == "add":
		name := ""
		if len(args) == 2 {
			name = args[1]
		}
		line, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<10))
		if err != nil {
			return err
		}
		k, err := s.Add(name, strings.TrimSpace(string(line)))
		if err != nil {
			return err
		}
		logf("added SSH key %s (%s)", k.Name, k.Fingerprint)
		return nil
	case len(args) == 1 && args[0] == "list":
		keys, err := s.List()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tFINGERPRINT\tADDED")
		for _, k := range keys {
			fmt.Fprintf(w, "%s\t%s\t%s\n", k.Name, k.Fingerprint, k.Added.Format("2006-01-02"))
		}
		return w.Flush()
	case len(args) == 2 && args[0] == "rm":
		return s.Remove(args[1])
	}
	return errors.New("usage: coolix ssh-key add [NAME] < key.pub, coolix ssh-key list, coolix ssh-key rm NAME")
}
