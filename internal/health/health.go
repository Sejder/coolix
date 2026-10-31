// Package health checks whether a freshly started app slot is ready.
package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Check probes 127.0.0.1:port once. With a path it expects an HTTP status
// below 400; without one, an accepted TCP connection is enough.
func Check(ctx context.Context, port int, path string) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if path == "" {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return c.Close()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return nil
}
