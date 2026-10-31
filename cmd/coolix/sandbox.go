package main

// Commands that run inside app and database units (as the app's DynamicUser,
// never as root). Root renders units that call them by a fixed path, so they
// replace the shell scripts the Nix lib used to generate.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"coolix/internal/query"
	"coolix/internal/secrets"
	"coolix/internal/units"
)

var sandboxCommands = map[string]func(ctx context.Context, args []string) error{
	"exec-app":    execApp,
	"db-init":     dbInit,
	"db-createdb": dbCreatedb,
	"db-backup":   dbBackup,
	"db-restore":  dbRestore,
	"query-serve": queryServe,
	// sshd's AuthorizedKeysCommand, also without coolix's environment.
	"ssh-authorized-keys": sshAuthorizedKeys,
}

// execApp sets the slot's PORT, loads secrets from the unit's credential and
// execs the app.
func execApp(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("exec-app", flag.ContinueOnError)
	slot := fs.String("slot", "", "blue or green")
	base := fs.Int("port-base", 0, "port of the blue slot")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 || (*slot != "blue" && *slot != "green") || *base <= 0 {
		return errors.New("usage: coolix exec-app --slot blue|green --port-base N -- COMMAND [ARGS]")
	}
	port := *base
	if *slot == "green" {
		port++
	}
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if dir := env["CREDENTIALS_DIRECTORY"]; dir != "" {
		data, err := os.ReadFile(filepath.Join(dir, secrets.CredName))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		vars, err := secrets.Decode(string(data))
		if err != nil {
			return fmt.Errorf("secrets: %w", err)
		}
		for k, v := range vars {
			if !units.ReservedEnv(k) {
				env[k] = v
			}
		}
	}
	if p := env["COOLIX_PATH"]; p != "" {
		if env["PATH"] != "" {
			p += ":" + env["PATH"]
		}
		env["PATH"] = p
	}
	delete(env, "COOLIX_PATH")
	env["PORT"] = strconv.Itoa(port)
	env["COOLIX_SLOT"] = *slot

	envv := make([]string, 0, len(env))
	for k, v := range env {
		envv = append(envv, k+"="+v)
	}
	argv := fs.Args()
	return syscall.Exec(argv[0], argv, envv)
}

func run1(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return nil
}

// dbInit creates the postgres cluster in $STATE_DIRECTORY on first start.
func dbInit(_ context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: coolix db-init POSTGRES_PACKAGE USER")
	}
	dir := os.Getenv("STATE_DIRECTORY")
	if dir == "" {
		return errors.New("STATE_DIRECTORY is not set")
	}
	if st, err := os.Stat(filepath.Join(dir, "PG_VERSION")); err == nil && st.Size() > 0 {
		return nil
	}
	return run1(args[0]+"/bin/initdb", "-D", dir, "-U", args[1], "--auth=peer", "--encoding=UTF8", "--locale=C")
}

// dbCreatedb creates the app's database once the server is up.
func dbCreatedb(_ context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: coolix db-createdb POSTGRES_PACKAGE SOCKET_DIR")
	}
	out, err := exec.Command(args[0]+"/bin/psql", "-h", args[1], "-d", "postgres", "-tAc",
		"SELECT 1 FROM pg_database WHERE datname = 'app'").Output()
	if err != nil {
		return fmt.Errorf("psql: %w", err)
	}
	if strings.TrimSpace(string(out)) == "1" {
		return nil
	}
	return run1(args[0]+"/bin/createdb", "-h", args[1], "app")
}

// dbBackup dumps the app database into DIR and keeps the newest KEEP dumps.
func dbBackup(_ context.Context, args []string) error {
	if len(args) != 4 {
		return errors.New("usage: coolix db-backup POSTGRES_PACKAGE SOCKET_DIR DIR KEEP")
	}
	pkg, sock, dir := args[0], args[1], args[2]
	keep, err := strconv.Atoi(args[3])
	if err != nil || keep < 1 {
		return fmt.Errorf("invalid KEEP %q", args[3])
	}
	// Leftovers of a killed dump.
	if stale, _ := filepath.Glob(filepath.Join(dir, ".*.dump.tmp")); len(stale) > 0 {
		for _, f := range stale {
			os.Remove(f)
		}
	}
	// Names have one-second resolution; never overwrite an existing dump.
	var name string
	for {
		name = time.Now().UTC().Format("20060102T150405Z") + ".dump"
		if _, err := os.Lstat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := run1(pkg+"/bin/pg_dump", "-h", sock, "--format=custom", "--file="+tmp, "app"); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
		fmt.Fprintf(os.Stderr, "backup %s written (%d bytes)\n", name, st.Size())
	}
	files, err := query.ListBackups(dir)
	if err != nil {
		return err
	}
	for _, f := range files[min(keep, len(files)):] {
		fmt.Fprintf(os.Stderr, "removing old backup %s\n", f.Name)
		if err := os.Remove(filepath.Join(dir, f.Name)); err != nil {
			return err
		}
	}
	return nil
}

// dbRestore replaces the app database's contents with a dump.
func dbRestore(_ context.Context, args []string) error {
	if len(args) != 3 {
		return errors.New("usage: coolix db-restore POSTGRES_PACKAGE SOCKET_DIR FILE")
	}
	pkg, sock, file := args[0], args[1], args[2]
	if !units.ValidBackupFile(filepath.Base(file)) {
		return fmt.Errorf("%q is not a coolix backup", filepath.Base(file))
	}
	if _, err := os.Stat(file); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "restoring %s\n", filepath.Base(file))
	if err := run1(pkg+"/bin/pg_restore", "-h", sock, "--dbname=app", "--clean", "--if-exists",
		"--no-owner", "--single-transaction", file); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "restore complete")
	return nil
}

func queryServe(ctx context.Context, _ []string) error {
	dir := os.Getenv("COOLIX_BACKUP_DIR")
	if dir == "" {
		dir = "/run/coolix-backups"
	}
	return query.Serve(ctx, os.Stdin, os.Stdout, &query.Server{BackupDir: dir})
}
