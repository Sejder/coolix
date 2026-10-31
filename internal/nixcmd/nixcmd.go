// Package nixcmd wraps the nix CLI calls coolix makes.
package nixcmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

var baseArgs = []string{"--extra-experimental-features", "nix-command flakes"}

func run(ctx context.Context, dir string, logw io.Writer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "nix", append(append([]string{}, baseArgs...), args...)...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	if logw == nil {
		logw = os.Stderr
	}
	cmd.Stderr = logw
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("nix %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out.String()), nil
}

// Lock adds/removes inputs so flake.lock matches flake.nix, then updates the
// named inputs to their latest revisions.
func Lock(ctx context.Context, flakeDir string, logw io.Writer, update ...string) error {
	if _, err := run(ctx, flakeDir, logw, "flake", "lock"); err != nil {
		return err
	}
	if len(update) == 0 {
		return nil
	}
	_, err := run(ctx, flakeDir, logw, append([]string{"flake", "update"}, update...)...)
	return err
}

// Build builds a flake output, registers outLink as a GC root for it and
// returns its store path.
func Build(ctx context.Context, installable, outLink string, logw io.Writer) (string, error) {
	return run(ctx, "", logw, "build", "--out-link", outLink, "--print-out-paths", "-L", installable)
}

// SetProfile points a profile at storePath, creating a new generation.
func SetProfile(ctx context.Context, profile, storePath string) error {
	cmd := exec.CommandContext(ctx, "nix-env", "--profile", profile, "--set", storePath)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nix-env --set: %w", err)
	}
	return nil
}

// Rollback switches the profile to its previous generation.
func Rollback(ctx context.Context, profile string) error {
	return nixEnv(ctx, "--profile", profile, "--rollback")
}

// DeleteOldGenerations keeps the newest keep generations of profile.
func DeleteOldGenerations(ctx context.Context, profile string, keep int) error {
	return nixEnv(ctx, "--profile", profile, "--delete-generations", fmt.Sprintf("+%d", keep))
}

func nixEnv(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "nix-env", args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nix-env %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
