package adapters

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/google/uuid"
)

func TestDockerHTTPOverNamedPipe(t *testing.T) {
	name := "knotra-test-" + uuid.NewString()
	listener, err := winio.ListenPipe(`\\.\pipe\`+name, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/info" {
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"OSType":"linux","Architecture":"x86_64"}`))
	})}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Docker fixture server: %v", err)
		}
	}()
	t.Cleanup(func() { _ = server.Close() })
	runner := &Runner{DockerHost: "npipe:////./pipe/" + name}
	docker, err := runner.docker(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer docker.client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var info struct{ OSType, Architecture string }
	if err := docker.json(ctx, "GET", "/info", nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.OSType != "linux" || info.Architecture != "x86_64" {
		t.Fatalf("invalid Docker response: %+v", info)
	}
}
