package adapters

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
)

type dockerClient struct {
	client *http.Client
	base   string
}

type dockerAPIError struct {
	status       int
	method, path string
}

func (e *dockerAPIError) Error() string {
	return fmt.Sprintf("Docker %s %s returned HTTP %d", e.method, e.path, e.status)
}

func (d *dockerClient) remove(ctx context.Context, id string) error {
	err := d.json(ctx, "DELETE", "/containers/"+url.PathEscape(id)+"?force=true", nil, nil)
	var apiErr *dockerAPIError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
		return nil
	}
	return err
}

func (r *Runner) docker() (*dockerClient, error) {
	host := r.DockerHost
	if host == "" {
		host = os.Getenv("DOCKER_HOST")
	}
	if host == "" {
		// The Docker context selects a socket only. No workload runs on the host.
		out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err == nil {
			host = strings.TrimSpace(string(out))
		}
	}
	if host == "" {
		host = defaultDockerHost
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, err
	}
	dial, err := dockerDialer(u)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{IdleConnTimeout: 30 * time.Second, MaxIdleConns: 2, MaxIdleConnsPerHost: 2, DialContext: dial}
	return &dockerClient{client: &http.Client{Transport: transport}, base: "http://docker"}, nil
}

type dockerDialFunc func(context.Context, string, string) (net.Conn, error)

func dockerDialer(u *url.URL) (dockerDialFunc, error) {
	if u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("Docker host must be a local unix socket or named pipe")
	}
	switch u.Scheme {
	case "unix":
		if !strings.HasPrefix(u.Path, "/") || u.Path == "/" {
			return nil, fmt.Errorf("Docker unix socket path must be absolute")
		}
		return func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", u.Path)
		}, nil
	case "npipe":
		pipe, err := dockerPipePath(u.Path)
		if err != nil {
			return nil, err
		}
		return namedPipeDialer(pipe)
	default:
		return nil, fmt.Errorf("Docker host must be a local unix socket or named pipe")
	}
}

func dockerPipePath(path string) (string, error) {
	name, ok := strings.CutPrefix(path, "//./pipe/")
	if !ok || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return "", fmt.Errorf("Docker named pipe must use npipe:////./pipe/NAME")
	}
	return `\\.\pipe\` + name, nil
}

func (d *dockerClient) request(ctx context.Context, method, path string, input any) (*http.Response, error) {
	var reader io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Docker request: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		return nil, &dockerAPIError{res.StatusCode, method, strings.Split(path, "?")[0]}
	}
	return res, nil
}

func (d *dockerClient) json(ctx context.Context, method, path string, input, output any) error {
	res, err := d.request(ctx, method, path, input)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(output)
}

type sandbox struct {
	docker    *dockerClient
	id, dir   string
	env       []string
	inputs    contract.Values
	stopLease context.CancelFunc
	leaseDone chan struct{}
}

func (s *sandbox) close() {
	defer s.docker.client.CloseIdleConnections()
	if s.stopLease != nil {
		s.stopLease()
		<-s.leaseDone
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = s.docker.remove(ctx, s.id)
	_ = os.RemoveAll(s.dir)
}

type dockerMount struct {
	Type     string
	Source   string
	Target   string
	ReadOnly bool
}

func (r *Runner) newSandbox(ctx context.Context, req Request, profile contract.SandboxProfile, environment []string) (*sandbox, error) {
	if r.HelperPath == "" || r.WorkDir == "" {
		return nil, fmt.Errorf("sandbox helper path and work directory must be configured")
	}
	helper, err := filepath.Abs(r.HelperPath)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(helper); err != nil {
		return nil, fmt.Errorf("sandbox helper is unavailable")
	}
	if expected := req.Plan.Runtime.HelperSHA256; expected != "" {
		data, err := os.ReadFile(helper)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != expected {
			return nil, fmt.Errorf("sandbox helper changed after admission")
		}
	}
	if err = os.MkdirAll(r.workRoot(), 0700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(r.workRoot(), "sandbox-")
	if err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	s := &sandbox{
		dir:    dir,
		env:    append([]string{"KNOTRA_INPUT_JSON=/knotra/input.json", "KNOTRA_OUTPUT_JSON=/knotra/output.json"}, environment...),
		inputs: contract.Values{},
	}
	ok := false
	defer func() {
		if !ok {
			if s.stopLease != nil {
				s.stopLease()
				<-s.leaseDone
				s.stopLease = nil
			}
			if s.id != "" {
				s.close()
			} else {
				_ = os.RemoveAll(dir)
			}
		}
	}()
	s.docker, err = r.docker()
	if err != nil {
		return nil, err
	}
	var deadline time.Time
	if !req.service && (req.Node.Type == "agent" || req.Node.Type == "code") {
		var bounded bool
		deadline, bounded = ctx.Deadline()
		if !bounded {
			deadline = time.Now().Add(30 * time.Minute)
		}
	}
	if err = s.startLease(deadline); err != nil {
		return nil, err
	}
	packageDir := filepath.Join(dir, "package")
	if err = os.Mkdir(packageDir, 0755); err != nil {
		return nil, err
	}
	requestDir := filepath.Join(dir, "requests")
	if err = os.Mkdir(requestDir, 0755); err != nil {
		return nil, err
	}

	for _, file := range req.Plan.Package.Files {
		name := filepath.Join(packageDir, filepath.FromSlash(file.Path))
		if !strings.HasPrefix(name, packageDir+string(filepath.Separator)) {
			return nil, fmt.Errorf("unsafe package path")
		}
		if err = os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			return nil, err
		}
		if err = os.WriteFile(name, file.Content, 0444); err != nil {
			return nil, err
		}
	}

	mounts := []dockerMount{
		{"bind", helper, "/knotra/bin/helper", true},
		{"bind", packageDir, "/package", true},
		{"bind", requestDir, "/knotra/requests", true},
		{"bind", filepath.Join(dir, "control"), "/knotra/control", true},
	}

	for name, value := range req.Inputs {
		if value.JSON != nil {
			s.inputs[name] = value
			continue
		}
		port := req.Node.Inputs[name]
		copied := value
		copied.Artifacts = append([]contract.Artifact(nil), value.Artifacts...)
		mountDir := filepath.Join(dir, "input-"+name)
		if err = os.Mkdir(mountDir, 0755); err != nil {
			return nil, err
		}

		for i, artifact := range value.Artifacts {
			data, err := r.Hooks.GetArtifact(ctx, artifact.ID)
			if err != nil {
				return nil, err
			}
			filename := fmt.Sprint(i)
			target := "/workspace/" + port.Mount
			if value.Collection {
				target += "/" + filename
			}
			if err = os.WriteFile(filepath.Join(mountDir, filename), data, 0444); err != nil {
				return nil, err
			}
			copied.Artifacts[i].Path = target
		}

		if value.Collection {
			mounts = append(mounts, dockerMount{"bind", mountDir, "/workspace/" + port.Mount, true})
		} else {
			mounts = append(mounts, dockerMount{"bind", filepath.Join(mountDir, "0"), "/workspace/" + port.Mount, true})
		}
		s.inputs[name] = copied
	}

	input, err := json.Marshal(contract.ContextEnvelope(s.inputs))
	if err != nil {
		return nil, err
	}
	inputPath := filepath.Join(dir, "input.json")
	if err = os.WriteFile(inputPath, input, 0444); err != nil {
		return nil, err
	}
	mounts = append(mounts, dockerMount{"bind", inputPath, "/knotra/input.json", true})
	disk := profile.Resources.DiskMiB << 20
	if disk < 1<<20 {
		return nil, fmt.Errorf("sandbox disk limit too small")
	}
	small := disk / 20
	workspace := disk - 2*small - 4096
	network := "none"
	if profile.Network.Mode != "none" {
		network = "bridge"
	}
	extraHosts := []string{}
	ips := []string{}
	if profile.Network.Mode == "allowlist" {
		for _, host := range profile.Network.Hosts {
			addresses := profile.ResolvedHosts[host]
			if len(addresses) == 0 {
				return nil, fmt.Errorf("allowlist addresses were not snapshotted")
			}

			for _, address := range addresses {
				ip := net.ParseIP(address)
				if ip == nil {
					return nil, fmt.Errorf("invalid resolved IP")
				}
				ips = append(ips, ip.String())
				if net.ParseIP(host) == nil {
					extraHosts = append(extraHosts, host+":"+ip.String())
				}
			}
		}
	}
	tmpfs := func(size int64) string {
		return fmt.Sprintf("rw,nosuid,nodev,size=%d,uid=65532,gid=65532,mode=0700", size)
	}
	body := map[string]any{
		"Image":      profile.Image,
		"Entrypoint": []string{"/knotra/bin/helper", "init"},
		"Cmd":        []string{},
		"User":       "65532:65532",
		"WorkingDir": "/workspace",
		"Env":        s.env,
		"Labels": map[string]string{
			"io.knotra.engine":   r.EngineID,
			"io.knotra.run":      req.RunID,
			"io.knotra.instance": req.InstanceID,
		},
		"HostConfig": map[string]any{
			"ReadonlyRootfs": true,
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges:true"},
			"NetworkMode":    network,
			"Mounts":         mounts,
			"Tmpfs": map[string]string{
				"/workspace": tmpfs(workspace),
				"/knotra":    tmpfs(small),
				"/tmp":       tmpfs(small),
				"/dev/shm":   tmpfs(4096),
			},
			"ShmSize":    int64(0),
			"Memory":     profile.Resources.MemoryMiB << 20,
			"MemorySwap": profile.Resources.MemoryMiB << 20,
			"NanoCpus":   int64(profile.Resources.CPU * 1e9),
			"PidsLimit":  profile.Resources.Pids,
			"ExtraHosts": extraHosts,
			"LogConfig":  map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "16k", "max-file": "1"}},
		},
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err = s.docker.json(ctx, "POST", "/containers/create", body, &created); err != nil {
		return nil, err
	}
	s.id = created.ID
	if err = s.docker.json(ctx, "POST", "/containers/"+s.id+"/start", nil, nil); err != nil {
		return nil, err
	}
	if profile.Network.Mode == "allowlist" {
		if err = r.installFirewall(ctx, s, ips, req.Plan.Runtime.FirewallImage); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}

func (r *Runner) installFirewall(ctx context.Context, s *sandbox, ips []string, image string) error {
	if image == "" {
		return fmt.Errorf("allowlist requires configured firewall image")
	}
	sort.Strings(ips)
	ips = slices.Compact(ips)
	script := "set -eu\niptables -P OUTPUT DROP\niptables -P FORWARD DROP\nip6tables -P OUTPUT DROP\nip6tables -P FORWARD DROP\n"

	for _, tool := range []string{"iptables", "ip6tables"} {
		script += tool + " -A OUTPUT -p udp --dport 53 -j DROP\n" + tool + " -A OUTPUT -p tcp --dport 53 -j DROP\n" + tool + " -A OUTPUT -o lo -j ACCEPT\n" + tool + " -A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT\n"
	}

	for _, address := range ips {
		tool := "iptables"
		if strings.Contains(address, ":") {
			tool = "ip6tables"
		}

		for _, protocol := range []string{"tcp", "udp"} {
			script += tool + " -A OUTPUT -d " + address + " -p " + protocol + " -j ACCEPT\n"
		}
	}

	body := map[string]any{
		"Image":      image,
		"Entrypoint": []string{"/bin/sh", "-c", script},
		"Labels":     map[string]string{"io.knotra.engine": r.EngineID},
		"HostConfig": map[string]any{
			"NetworkMode":    "container:" + s.id,
			"ReadonlyRootfs": true,
			"CapDrop":        []string{"ALL"},
			"CapAdd":         []string{"NET_ADMIN"},
			"SecurityOpt":    []string{"no-new-privileges:true"},
			"Memory":         32 << 20,
			"PidsLimit":      16,
		},
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := s.docker.json(ctx, "POST", "/containers/create", body, &created); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.docker.json(cleanup, "DELETE", "/containers/"+created.ID+"?force=true", nil, nil)
	}()
	if err := s.docker.json(ctx, "POST", "/containers/"+created.ID+"/start", nil, nil); err != nil {
		return err
	}
	var status struct{ StatusCode int }
	if err := s.docker.json(ctx, "POST", "/containers/"+created.ID+"/wait?condition=not-running", nil, &status); err != nil {
		return err
	}
	if status.StatusCode != 0 {
		return fmt.Errorf("sandbox firewall initialization failed")
	}
	return nil
}

func (s *sandbox) helper(ctx context.Context, operation string, payload any) (json.RawMessage, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(filepath.Join(s.dir, "requests"), "request-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0444); err != nil {
		file.Close()
		return nil, err
	}
	if _, err = file.Write(b); err != nil {
		file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	cmd := []string{"/knotra/bin/helper", operation, "@/knotra/requests/" + filepath.Base(file.Name())}
	var created struct {
		ID string `json:"Id"`
	}
	body := map[string]any{
		"AttachStdout": true,
		"AttachStderr": true,
		"Cmd":          cmd,
		"Env":          s.env,
		"User":         "65532:65532",
		"WorkingDir":   "/workspace",
	}
	if err = s.docker.json(ctx, "POST", "/containers/"+s.id+"/exec", body, &created); err != nil {
		return nil, s.diagnose(err)
	}
	res, err := s.docker.request(ctx, "POST", "/exec/"+created.ID+"/start", map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return nil, s.diagnose(err)
	}
	defer res.Body.Close()
	stdout, _, err := demultiplex(res.Body, 360<<20)
	if err != nil {
		return nil, s.diagnose(err)
	}
	var inspection struct {
		ExitCode int
		Running  bool
	}
	if err = s.docker.json(ctx, "GET", "/exec/"+created.ID+"/json", nil, &inspection); err != nil {
		return nil, s.diagnose(err)
	}
	if inspection.Running || inspection.ExitCode != 0 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(stdout, &e)
		if e.Error == "" {
			e.Error = "sandbox helper failed"
		}
		return nil, fmt.Errorf("%s", e.Error)
	}
	return stdout, nil
}

func demultiplex(reader io.Reader, limit int64) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	var header [8]byte
	var total int64

	for {
		_, err := io.ReadFull(reader, header[:])
		if err == io.EOF {
			return stdout.Bytes(), stderr.Bytes(), nil
		}
		if err != nil {
			return nil, nil, err
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		total += size
		if total > limit {
			return nil, nil, fmt.Errorf("Docker output exceeds limit")
		}
		var out io.Writer

		switch header[0] {
		case 1:
			out = &stdout
		case 2:
			out = &stderr
		default:
			return nil, nil, fmt.Errorf("invalid Docker stream")
		}

		if _, err = io.CopyN(out, reader, size); err != nil {
			return nil, nil, err
		}
	}
}

func (r *Runner) nodeSandbox(ctx context.Context, req Request) (*sandbox, error) {
	resource := pipeline(req).Spec.Sandboxes[req.Node.Sandbox]
	profile, ok := req.Plan.Profile.Spec.Sandboxes[resource.Profile]
	if !ok {
		return nil, fmt.Errorf("sandbox profile missing")
	}
	environment := []string{}

	for name, value := range req.Node.Env {
		if strings.HasPrefix(name, "KNOTRA_") {
			return nil, fmt.Errorf("reserved environment variable")
		}
		text := ""
		if value.Value != nil {
			text = *value.Value
		} else {
			secret := pipeline(req).Spec.Secrets[value.Secret].Ref
			if !slices.Contains(profile.AllowedSecrets, secret) {
				return nil, fmt.Errorf("sandbox secret is forbidden")
			}
			var err error
			text, err = r.secret(req.Plan.Profile, secret)
			if err != nil {
				return nil, err
			}
		}
		environment = append(environment, name+"="+text)
	}

	sort.Strings(environment)
	return r.newSandbox(ctx, req, profile, environment)
}

func (r *Runner) collect(ctx context.Context, req Request, s *sandbox, values contract.Values) (contract.Values, error) {
	for name, port := range req.Node.Outputs {
		if port.Artifact == nil {
			continue
		}
		b, err := s.helper(ctx, "collect", map[string]any{"path": port.Collect.Path, "encoding": "base64"})
		if err != nil {
			return nil, failure("OUTPUT_INVALID", err)
		}
		var file struct {
			Content string `json:"content"`
		}
		if err = json.Unmarshal(b, &file); err != nil {
			return nil, err
		}
		data, err := base64.StdEncoding.DecodeString(file.Content)
		if err != nil {
			return nil, err
		}
		artifact, err := r.Hooks.PutArtifact(ctx, path.Base(port.Collect.Path), port.Collect.MediaType, data)
		if err != nil {
			return nil, err
		}
		values[name] = contract.Value{Artifacts: []contract.Artifact{artifact}}
	}

	return values, nil
}

func (r *Runner) code(ctx context.Context, req Request) (contract.Values, error) {
	op := Operation{ID: operationID(req, "code", true), Kind: "tool", Effect: "unknown"}
	state, err := r.Hooks.BeginOperation(ctx, op)
	if err != nil {
		return nil, err
	}
	if state.Completed {
		var values contract.Values
		err = json.Unmarshal(state.Response, &values)
		return values, err
	}
	if state.Started {
		return nil, &Failure{
			Code:        "OUTCOME_UNKNOWN",
			Message:     "code attempt started without durable final outputs",
			OperationID: op.ID,
			Unknown:     true,
		}
	}
	s, err := r.nodeSandbox(ctx, req)
	if err != nil {
		return nil, &Failure{Code: "SANDBOX_FAILED", Message: err.Error(), Retryable: true}
	}
	defer s.close()
	if err = r.Hooks.Reserve(ctx, "tool"); err != nil {
		return nil, err
	}
	timeout := "30m"
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline).String()
	}
	raw, err := s.helper(ctx, "exec", map[string]any{"command": req.Node.Code.Command, "timeout": timeout})
	if err != nil {
		return nil, &Failure{Code: "OUTCOME_UNKNOWN", Message: err.Error(), OperationID: op.ID, Unknown: true}
	}
	var process struct {
		ExitCode int `json:"exitCode"`
	}
	if err = json.Unmarshal(raw, &process); err != nil {
		return nil, err
	}
	if process.ExitCode != 0 {
		return nil, failure("PROCESS_FAILED", fmt.Errorf("process exited with code %d", process.ExitCode))
	}
	b, err := s.helper(ctx, "collect", map[string]any{"path": "output.json", "root": "output", "encoding": "utf8"})
	if err != nil {
		return nil, failure("OUTPUT_INVALID", err)
	}
	var file struct{ Content string }
	if err = json.Unmarshal(b, &file); err != nil {
		return nil, err
	}
	values, err := jsonOutputs(req.Node.Outputs, []byte(file.Content))
	if err != nil {
		return nil, err
	}
	values, err = r.collect(ctx, req, s, values)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	if err = r.Hooks.CompleteOperation(ctx, op.ID, data); err != nil {
		return nil, &Failure{
			Code:        "OUTCOME_UNKNOWN",
			Message:     "cannot persist code outputs",
			OperationID: op.ID,
			Unknown:     true,
		}
	}
	return values, nil
}
