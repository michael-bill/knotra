package adapters

import (
	"net/url"
	"testing"
)

func TestDockerTransportRejectsRemoteEndpoints(t *testing.T) {
	for _, host := range []string{
		"tcp://127.0.0.1:2375", "https://docker.example", "unix://remote/var/run/docker.sock",
		"unix:relative", "unix:///", "unix:///socket?token=secret",
		"npipe:////remote/pipe/docker_engine", "npipe:////./pipe/", "npipe:////./pipe/../other",
		"npipe:////./pipe/docker_engine%5cother", "npipe:////./pipe/docker_engine%00",
	} {
		t.Run(host, func(t *testing.T) {
			u, err := url.Parse(host)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := dockerDialer(u); err == nil {
				t.Fatalf("unsafe Docker endpoint accepted: %s", host)
			}
		})
	}
}

func TestDockerDesktopPipePaths(t *testing.T) {
	for _, name := range []string{"docker_engine", "dockerDesktopLinuxEngine"} {
		got, err := dockerPipePath("//./pipe/" + name)
		if err != nil || got != `\\.\pipe\`+name {
			t.Fatalf("pipe path: %q, %v", got, err)
		}
	}
}
