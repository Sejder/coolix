// Package caddy writes per-app Caddy site snippets into a directory the host
// Caddyfile imports, and reloads Caddy when they change.
package caddy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type TLSMode string

const (
	TLSAcme     TLSMode = "acme"     // Caddy's automatic HTTPS
	TLSInternal TLSMode = "internal" // Caddy's local CA
	TLSOff      TLSMode = "off"      // plain HTTP
)

type Router struct {
	Dir    string
	TLS    TLSMode
	Reload func() error
}

func SystemdReload() error {
	if out, err := exec.Command("systemctl", "reload", "caddy.service").CombinedOutput(); err != nil {
		return fmt.Errorf("reloading caddy: %w\n%s", err, out)
	}
	return nil
}

// Render returns the site block routing domains to 127.0.0.1:port.
func Render(domains []string, port int, tls TLSMode) string {
	addrs := make([]string, len(domains))
	for i, d := range domains {
		if tls == TLSOff {
			d = "http://" + d
		}
		addrs[i] = d
	}
	var b strings.Builder
	b.WriteString("# Managed by coolix.\n")
	fmt.Fprintf(&b, "%s {\n", strings.Join(addrs, ", "))
	if tls == TLSInternal {
		b.WriteString("\ttls internal\n")
	}
	fmt.Fprintf(&b, "\treverse_proxy 127.0.0.1:%d\n}\n", port)
	return b.String()
}

func (r *Router) path(app string) string { return filepath.Join(r.Dir, app+".caddy") }

// SetRoute points app's domains at port. No domains removes the route.
// Caddy is only reloaded when the snippet changes; if the reload fails the
// previous snippet is restored.
func (r *Router) SetRoute(app string, domains []string, port int) error {
	if len(domains) == 0 {
		return r.RemoveRoute(app)
	}
	content := []byte(Render(domains, port, r.TLS))
	p := r.path(app)
	old, err := os.ReadFile(p)
	if err == nil && bytes.Equal(old, content) {
		return nil
	}
	if err := r.write(p, content); err != nil {
		return err
	}
	if err := r.Reload(); err != nil {
		if old != nil {
			_ = r.write(p, old)
		} else {
			_ = os.Remove(p)
		}
		return err
	}
	return nil
}

func (r *Router) RemoveRoute(app string) error {
	err := os.Remove(r.path(app))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return r.Reload()
}

// Port returns the port app's route currently points at, or 0.
func (r *Router) Port(app string) int {
	data, err := os.ReadFile(r.path(app))
	if err != nil {
		return 0
	}
	var port int
	for _, line := range strings.Split(string(data), "\n") {
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "reverse_proxy 127.0.0.1:%d", &port); err == nil {
			return port
		}
	}
	return 0
}

func (r *Router) write(p string, content []byte) error {
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
