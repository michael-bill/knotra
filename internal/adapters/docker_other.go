//go:build !windows

package adapters

import "fmt"

const defaultDockerHost = "unix:///var/run/docker.sock"

func namedPipeDialer(string) (dockerDialFunc, error) {
	return nil, fmt.Errorf("docker named pipes require Windows")
}
