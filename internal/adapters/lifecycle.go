package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/michael-bill/knotra/internal/execution"
)

const sandboxLeaseTTL = 45 * time.Second

const sandboxHeartbeat = 5 * time.Second

func (r *Runner) workRoot() string {
	if r.EngineID == "" {
		return r.WorkDir
	}
	sum := sha256.Sum256([]byte(r.EngineID))
	root := filepath.Join(r.WorkDir, "engine-"+hex.EncodeToString(sum[:16]))
	// Preserve the existing single-host layout. Explicit hosts get separate
	// staging namespaces even when their configured work directories overlap.
	if r.HostID != "" && r.HostID != "local" {
		host := sha256.Sum256([]byte(r.HostID))
		root = filepath.Join(root, "host-"+hex.EncodeToString(host[:16]))
	}
	return root
}

// CleanupOwned is called once after acquiring the engine's exclusive process
// lease, before starting workers. It never selects another engine's containers
// or resources with River ownership. The optional database check protects even
// directories whose container creation was interrupted or already removed.
// An abrupt engine exit can leave host package directories even though container
// watchdogs stop its processes independently of this cleanup.
func (r *Runner) CleanupOwned(ctx context.Context, protected func(context.Context, string) (bool, error)) error {
	if r.EngineID == "" || r.WorkDir == "" {
		return fmt.Errorf("cleanup requires engine identity and work directory")
	}
	docker, err := r.docker(ctx)
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
			return fmt.Errorf("docker returned an unexpected container identity during cleanup")
		}
		if container.Labels["io.knotra.worker"] != "" {
			continue
		}
		if protected != nil {
			keep, err := protected(ctx, container.Labels["io.knotra.resource"])
			if err != nil {
				return err
			}
			if keep {
				continue
			}
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
		if id, sandbox := strings.CutPrefix(entry.Name(), "sandbox-"); sandbox {
			if protected != nil {
				keep, err := protected(ctx, id)
				if err != nil {
					return err
				}
				if keep {
					continue
				}
			}
			if err = os.RemoveAll(filepath.Join(r.workRoot(), entry.Name())); err != nil {
				return err
			}
		}
	}

	return nil
}

// ResourcePage uses Docker's bounded container inventory. A removed pagination
// anchor resets the scan; no physical deletion is authorized by discovery alone.
func (r *Runner) ResourcePage(ctx context.Context, before string) ([]execution.ResourceRecord, string, error) {
	if r.EngineID == "" || r.HostID == "" {
		return nil, "", fmt.Errorf("resource inventory requires engine and host identity")
	}
	docker, err := r.docker(ctx)
	if err != nil {
		return nil, before, err
	}
	defer docker.client.CloseIdleConnections()
	labels := []string{"io.knotra.engine=" + r.EngineID, "io.knotra.host=" + r.HostID}
	filters := map[string][]string{"label": labels}
	if before != "" {
		filters["before"] = []string{before}
	}
	encoded, err := json.Marshal(filters)
	if err != nil {
		return nil, before, err
	}
	var containers []struct {
		ID     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
	}
	if err := docker.json(ctx, "GET", "/containers/json?all=true&limit=64&filters="+url.QueryEscape(string(encoded)), nil, &containers); err != nil {
		var apiErr *dockerAPIError
		if before != "" && errors.As(err, &apiErr) && (apiErr.status == 400 || apiErr.status == 404) {
			return nil, "", err
		}
		return nil, before, err
	}
	if len(containers) > 64 {
		return nil, before, fmt.Errorf("docker exceeded the resource inventory page limit")
	}
	next := ""
	if len(containers) == 64 {
		next = containers[len(containers)-1].ID
	}
	var resources []execution.ResourceRecord
	var invalid error
	for _, container := range containers {
		labels := container.Labels
		number, numberErr := strconv.Atoi(labels["io.knotra.attempt"])
		generation, generationErr := strconv.ParseInt(labels["io.knotra.generation"], 10, 64)
		resource := execution.ResourceRecord{ID: labels["io.knotra.resource"], EngineID: labels["io.knotra.engine"], HostID: labels["io.knotra.host"], Kind: "sandbox",
			Ownership: execution.Ownership{AttemptID: execution.AttemptID{RunID: labels["io.knotra.run"], InstanceID: labels["io.knotra.instance"], Number: number}, WorkerID: labels["io.knotra.worker"], Generation: generation}}
		if container.ID == "" || resource.EngineID != r.EngineID || resource.HostID != r.HostID || numberErr != nil || generationErr != nil || resource.Validate() != nil {
			invalid = errors.Join(invalid, fmt.Errorf("resource inventory received invalid container ownership"))
			continue
		}
		resources = append(resources, resource)
	}
	return resources, next, invalid
}

// CleanupResource requires a durable cleanup claim. Recheck every physical label
// before deleting anything; engine identity alone never proves abandonment.
func (r *Runner) CleanupResource(ctx context.Context, resource execution.ResourceRecord) error {
	if err := resource.Validate(); err != nil {
		return err
	}
	if resource.Kind != "sandbox" || resource.State != "cleaning" || r.WorkDir == "" || resource.EngineID != r.EngineID || resource.HostID != r.HostID {
		return fmt.Errorf("resource cleanup does not match this runner and host")
	}
	docker, err := r.docker(ctx)
	if err != nil {
		return err
	}
	defer docker.client.CloseIdleConnections()
	labels := resource.Labels()
	filters, err := json.Marshal(map[string][]string{"label": {
		"io.knotra.engine=" + resource.EngineID, "io.knotra.host=" + resource.HostID, "io.knotra.resource=" + resource.ID,
	}})
	if err != nil {
		return err
	}
	var containers []struct {
		ID     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
	}
	if err := docker.json(ctx, "GET", "/containers/json?all=true&filters="+url.QueryEscape(string(filters)), nil, &containers); err != nil {
		return err
	}
	for _, container := range containers {
		if container.ID == "" {
			return fmt.Errorf("resource cleanup received an empty container identity")
		}
		for key, value := range labels {
			if container.Labels[key] != value {
				return fmt.Errorf("resource cleanup received a foreign container identity")
			}
		}
	}
	for _, container := range containers {
		if err := docker.remove(ctx, container.ID); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(r.workRoot())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.RemoveAll(resource.Name())
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
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.WriteString(expires.UTC().Format(time.RFC3339Nano)); err != nil {
		_ = file.Close()
		return time.Time{}, err
	}
	mode := os.FileMode(0444)
	if runtime.GOOS == "windows" {
		// A read-only NTFS destination cannot be replaced by the next renewal.
		// The control directory is mounted read-only inside the sandbox.
		mode = 0644
	}
	if err = file.Chmod(mode); err != nil {
		_ = file.Close()
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
		defer func() { _ = res.Body.Close() }()
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
