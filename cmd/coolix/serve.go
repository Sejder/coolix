package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coolix/internal/engine"
	"coolix/internal/web"
)

func runServe(ctx context.Context, e *engine.Engine) error {
	listen := os.Getenv("COOLIX_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:8090"
	}
	// Optional: without it, the admin account is created on the setup page.
	pwFile := os.Getenv("COOLIX_ADMIN_PASSWORD_FILE")
	if pwFile != "" {
		if err := web.CheckPasswordFile(pwFile); err != nil {
			return err
		}
	}
	key, err := web.LoadSessionKey(filepath.Join(e.StateDir, "session.key"))
	if err != nil {
		return err
	}
	s := &web.Server{
		Config:          e.Config,
		PasswordFile:    pwFile,
		SessionKey:      key,
		SecureCookies:   os.Getenv("COOLIX_SECURE_COOKIES") == "1",
		WebAuthnOrigins: strings.Fields(os.Getenv("COOLIX_WEBAUTHN_ORIGINS")),
	}
	h, err := s.Handler()
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	logf("web UI listening on http://%s", listen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runSetupCode prints a one-time code that claims the admin account on the
// UI's setup page (or, after `coolix account reset`, re-claims it).
func runSetupCode(e *engine.Engine) error {
	code, err := web.NewSetupCode(e.StateDir)
	if err != nil {
		return err
	}
	fmt.Printf("Setup code: %s\n\nEnter it on the coolix setup page within %s.\n", code, web.SetupCodeTTL)
	return nil
}

func runAccount(e *engine.Engine, args []string) error {
	if len(args) != 1 || args[0] != "reset" {
		return errors.New("usage: coolix account reset")
	}
	if err := web.ResetAccount(e.StateDir); err != nil {
		return err
	}
	logf("removed the admin account's password and passkeys; run `coolix setup-code` to claim it again")
	return nil
}
