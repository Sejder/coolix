package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func port(t *testing.T, srv *httptest.Server) int {
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := port(t, srv)
	ctx := context.Background()
	if err := Check(ctx, p, "/healthz"); err != nil {
		t.Fatal(err)
	}
	if err := Check(ctx, p, "/missing"); err == nil {
		t.Fatal("404 should fail")
	}
	if err := Check(ctx, p, ""); err != nil {
		t.Fatal("tcp check:", err)
	}
	srv.Close()
	if err := Check(ctx, p, ""); err == nil {
		t.Fatal("closed port should fail")
	}
}
