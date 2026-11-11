package secretdriver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "p.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socket
}

func TestVersions(t *testing.T) {
	socket := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/SecretProvider.Versions" {
			http.NotFound(w, r)
			return
		}
		var req struct{ Secrets []Secret }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Secrets) != 2 || req.Secrets[0].Labels["ref"] != "op://V/db/password" {
			t.Errorf("unexpected request %+v", req)
		}
		_, _ = w.Write([]byte(`{"Versions":{"app_db":"i1@1"},"Errors":{"app_x":"not found"},"CacheTTLSeconds":60}`))
	})
	res, err := Versions(context.Background(), socket, []Secret{
		{Name: "app_db", Labels: map[string]string{"ref": "op://V/db/password"}},
		{Name: "app_x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Versions["app_db"] != "i1@1" || res.Errors["app_x"] != "not found" || res.CacheTTL != time.Minute {
		t.Fatalf("got %+v", res)
	}
}

func TestVersionsUnsupported(t *testing.T) {
	socket := serve(t, http.NotFound)
	if _, err := Versions(context.Background(), socket, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestVersionsError(t *testing.T) {
	socket := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"Err":"boom"}`))
	})
	if _, err := Versions(context.Background(), socket, nil); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected a plain error, got %v", err)
	}
}
