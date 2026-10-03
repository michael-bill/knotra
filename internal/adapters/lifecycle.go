package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sandboxLeaseTTL = 45 * time.Second

const sandboxHeartbeat = 5 * time.Second

func (r *Runner) workRoot() string {
	if r.EngineID == "" {
		return r.WorkDir
	}
	sum := sha256.Sum256([]byte(r.EngineID))
	return filepath.Join(r.WorkDir, "engine-"+hex.EncodeToString(sum[:16]))
}

// CleanupOwned is called once after acquiring the engine's exclusive process
// lease, before starting workers. It never selects another engine's containers.
// An abrupt engine exit can leave host package directories even though container
// watchdogs stop its processes independently of this cleanup.
func (r *Runner) CleanupOwned(ctx context.Context) error {
	if r.EngineID == "" || r.WorkDir == "" {
		return fmt.Errorf("cleanup requires engine identity and work directory")
	}
	docker, err := r.docker()
	if err != nil {
		return err
	}
	defer docker.client.CloseIdleConnections()
	filters, err := json.Marshal(map[string][]string{"label": {"io.knotra.engine=" + r.EngineID}})
	if err != nil {
		return err
	}
	var containers []struct {
		ID     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
	}
	if err = docker.json(ctx, "GET", "/containers/json?all=true&filters="+url.QueryEscape(string(filters)), nil, &containers); err != nil {
		return err
	}

	for _, container := range containers {
		if container.Labels["io.knotra.engine"] != r.EngineID || container.ID == "" {
			return fmt.Errorf("Docker returned an unexpected container identity during cleanup")
		}
		if err = docker.remove(ctx, container.ID); err != nil {
			return err
		}
	}

	entries, err := os.ReadDir(r.workRoot())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "sandbox-") {
			if err = os.RemoveAll(filepath.Join(r.workRoot(), entry.Name())); err != nil {
				return err
			}
		}
	}

	return nil
}

// The lease lives in a read-only directory mount. Workloads can observe it but
// cannot renew it, change the hard deadline or impersonate the host heartbeat.
func (s *sandbox) startLease(deadline time.Time) error {
	dir := filepath.Join(s.dir, "control")
	if err := os.Mkdir(dir, 0755); err != nil {
		return err
	}
	if !deadline.IsZero() {
		if err := os.WriteFile(filepath.Join(dir, "deadline"), []byte(deadline.UTC().Format(time.RFC3339Nano)), 0444); err != nil {
			return err
		}
	}
	expires, err := renewLease(dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stopLease = cancel
	s.leaseDone = make(chan struct{})
	go func() {
		defer close(s.leaseDone)
		keepLease(ctx, expires, sandboxHeartbeat, func() (time.Time, error) { return renewLease(dir) })
	}()
	return nil
}

func keepLease(ctx context.Context, expires time.Time, interval time.Duration, renew func() (time.Time, error)) {
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// Retry transient shared-filesystem errors only while the last
			// successfully issued lease remains valid. Never resurrect a lease.
			if !expires.After(time.Now()) {
				return
			}
			if next, err := renew(); err == nil {
				expires = next
			}
		}
	}
}

func renewLease(dir string) (time.Time, error) {
	expires := time.Now().Add(sandboxLeaseTTL)
	file, err := os.CreateTemp(dir, ".lease-")
	if err != nil {
		return time.Time{}, err
	}
	defer os.Remove(file.Name())
	if _, err = file.WriteString(expires.UTC().Format(time.RFC3339Nano)); err != nil {
		file.Close()
		return time.Time{}, err
	}
	if err = file.Chmod(0444); err != nil {
		file.Close()
		return time.Time{}, err
	}
	if err = file.Close(); err != nil {
		return time.Time{}, err
	}
	return expires, os.Rename(file.Name(), filepath.Join(dir, "lease"))
}

// A stopped container keeps only bounded supervisor diagnostics until Close or
// startup cleanup. Exec streams are separate and never enter the container log.
func (s *sandbox) diagnose(original error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var inspection struct {
		State struct {
			Running, OOMKilled bool
			ExitCode           int
		}
	}
	if err := s.docker.json(ctx, "GET", "/containers/"+s.id+"/json", nil, &inspection); err != nil || inspection.State.Running {
		return original
	}
	if inspection.State.OOMKilled {
		return fmt.Errorf("sandbox exceeded its memory limit")
	}
	res, err := s.docker.request(ctx, "GET", "/containers/"+s.id+"/logs?stdout=true&stderr=true&tail=8", nil)
	if err == nil {
		defer res.Body.Close()
		stdout, _, readErr := demultiplex(res.Body, 16<<10)
		if readErr == nil {
			var diagnostic struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(stdout, &diagnostic) == nil && diagnostic.Error != "" {
				return fmt.Errorf("sandbox supervisor stopped: %s", diagnostic.Error)
			}
		}
	}
	return fmt.Errorf("sandbox supervisor stopped with exit code %d", inspection.State.ExitCode)
}
