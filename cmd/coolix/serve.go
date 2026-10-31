package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"coolix/internal/engine"
	"coolix/internal/web"
)

func runServe(ctx context.Context, e *engine.Engine) error {
	listen := os.Getenv("COOLIX_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:8090"
	}
	pwFile := os.Getenv("COOLIX_ADMIN_PASSWORD_FILE")
	if pwFile == "" {
		return errors.New("COOLIX_ADMIN_PASSWORD_FILE must point at a file with the admin password")
	}
	key, err := web.LoadSessionKey(filepath.Join(e.StateDir, "session.key"))
	if err != nil {
		return err
	}
	s := &web.Server{
		Config:        e.Config,
		PasswordFile:  pwFile,
		SessionKey:    key,
		SecureCookies: os.Getenv("COOLIX_SECURE_COOKIES") == "1",
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
