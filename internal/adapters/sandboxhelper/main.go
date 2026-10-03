// sandboxhelper is a minimal trusted Linux process supervisor and file adapter.
// Build it for the Docker daemon's architecture, not for the client's OS.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const limit = 1 << 20

type request struct {
	Command  []string `json:"command"`
	Timeout  string   `json:"timeout,omitempty"`
	Path     string   `json:"path,omitempty"`
	Root     string   `json:"root,omitempty"`
	Encoding string   `json:"encoding,omitempty"`
	Content  string   `json:"content,omitempty"`
	Mode     string   `json:"mode,omitempty"`
}

type boundedBuffer struct {
	b         []byte
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.b)+n > limit {
		b.truncated = true
		p = p[:limit-len(b.b)]
	}
	b.b = append(b.b, p...)
	return n, nil
}

func (b *boundedBuffer) text() string {
	s := strings.ToValidUTF8(string(b.b), "�")

	for len(s) > limit {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}

	return s
}

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("missing operation"))
	}
	op := os.Args[1]
	if op == "init" {
		if err := supervise("/knotra/control"); err != nil {
			fatal(err)
		}
		return
	}
	var req request
	if len(os.Args) > 2 {
		var data []byte
		var err error
		if strings.HasPrefix(os.Args[2], "@/knotra/requests/") {
			data, err = os.ReadFile(strings.TrimPrefix(os.Args[2], "@"))
		} else {
			data, err = base64.StdEncoding.DecodeString(os.Args[2])
		}
		if err != nil {
			fatal(err)
		}
		if len(data) > 8<<20 {
			fatal(errors.New("request too large"))
		}
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&req); err != nil {
			fatal(err)
		}
	} else {
		fatal(errors.New("missing payload"))
	}
	var out any
	var err error

	switch op {
	case "exec":
		out, err = execute(req)
	case "read", "collect":
		out, err = readFile(req, op == "collect")
	case "write":
		out, err = writeFile(req)
	default:
		err = errors.New("unknown operation")
	}

	if err != nil {
		fatal(err)
	}
	if err = json.NewEncoder(os.Stdout).Encode(out); err != nil {
		os.Exit(1)
	}
}

// Exiting PID 1 makes the kernel terminate every process in this PID namespace.
// The workload cannot write these read-only mounted control files. A hard
// deadline is independent of lease renewal; loss of the engine also expires the
// lease, including while an untrusted command or MCP server is still running.
func supervise(control string) error {
	if err := protectSupervisor(); err != nil {
		return fmt.Errorf("cannot protect sandbox supervisor: %w", err)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGCHLD)
	defer signal.Stop(signals)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lease, err := loadLease(control, time.Now())
	if err != nil {
		return err
	}

	for {
		if err := lease.check(control, time.Now()); err != nil {
			return err
		}

		select {
		case sig := <-signals:
			if sig != syscall.SIGCHLD {
				return nil
			}
			for {
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if err != nil || pid <= 0 {
					break
				}
			}
		case <-ticker.C:
		}
	}
}

type supervisorLease struct {
	expires, deadline time.Time
	lastReadError     error
}

func readDeadline(control, name string) (time.Time, error) {
	b, err := os.ReadFile(filepath.Join(control, name))
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, string(b))
}

func loadLease(control string, now time.Time) (*supervisorLease, error) {
	expires, err := readDeadline(control, "lease")
	if err != nil {
		return nil, fmt.Errorf("sandbox initial lease is unavailable: %w", err)
	}
	deadline, err := readDeadline(control, "deadline")
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("sandbox deadline is unavailable: %w", err)
	}
	state := &supervisorLease{expires: now.Add(expires.Sub(now))}
	if !deadline.IsZero() {
		state.deadline = now.Add(deadline.Sub(now))
	}
	if err := state.check(control, now); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *supervisorLease) check(control string, now time.Time) error {
	if !s.deadline.IsZero() && !s.deadline.After(now) {
		return errors.New("sandbox hard deadline expired")
	}
	// Once issued, a lease remains valid until its verified expiry. A transient
	// read failure or stale inode from a shared filesystem cannot revoke it.
	if expires, err := readDeadline(control, "lease"); err != nil {
		s.lastReadError = err
	} else {
		s.lastReadError = nil
		if expires.After(s.expires) {
			s.expires = now.Add(expires.Sub(now))
		}
	}
	if !s.expires.After(now) {
		if s.lastReadError != nil {
			return fmt.Errorf("sandbox lease expired; last renewal unreadable: %w", s.lastReadError)
		}
		return errors.New("sandbox lease expired without a fresh host heartbeat")
	}
	return nil
}

func fatal(err error) {
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"error": err.Error()})
	os.Exit(1)
}

func execute(req request) (any, error) {
	if len(req.Command) == 0 || req.Command[0] == "" {
		return nil, errors.New("empty command")
	}
	d := 5 * time.Minute
	if req.Timeout != "" {
		var err error
		d, err = time.ParseDuration(req.Timeout)
		if err != nil || d <= 0 {
			return nil, errors.New("invalid timeout")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
	cmd.Dir = "/workspace"
	cmd.Stdin = nil
	configureProcess(cmd)
	var stdout, stderr boundedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if cleanupErr := quiesce(); cleanupErr != nil {
		return nil, cleanupErr
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		err = nil
	}
	exit := 0
	if err != nil {
		var e *exec.ExitError
		if !errors.As(err, &e) {
			return nil, err
		}
		exit = e.ExitCode()
	}
	return map[string]any{
		"exitCode":  exit,
		"stdout":    stdout.text(),
		"stderr":    stderr.text(),
		"truncated": stdout.truncated || stderr.truncated,
	}, nil
}

func safePath(root, name string, createParents bool) (string, error) {
	if name == "" || filepath.IsAbs(name) || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
		return "", errors.New("invalid relative path")
	}
	parts := strings.Split(name, "/")
	current := root

	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("invalid path component")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) && createParents && i < len(parts)-1 {
			if err = os.Mkdir(current, 0700); err != nil {
				return "", err
			}
			continue
		}
		if os.IsNotExist(err) && i == len(parts)-1 {
			return current, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symbolic links are forbidden")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", errors.New("parent is not a directory")
		}
	}

	return current, nil
}

func readFile(req request, collect bool) (any, error) {
	root := "/workspace"
	if req.Root == "package" {
		root = "/package"
	} else if req.Root == "output" && collect {
		root = "/knotra"
	} else if req.Root != "" && req.Root != "workspace" {
		return nil, errors.New("invalid root")
	}
	name, err := safePath(root, req.Path, false)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || hasMultipleLinks(info) {
		return nil, errors.New("expected a regular file with one link")
	}
	max := int64(limit)
	if collect {
		max = 64 << 20
	}
	if info.Size() > max {
		return nil, errors.New("file exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("file exceeds size limit")
	}
	encoding := req.Encoding
	if encoding == "" {
		encoding = "utf8"
	}
	var content string

	switch encoding {
	case "base64":
		content = base64.StdEncoding.EncodeToString(data)
	case "utf8":
		if !utf8.Valid(data) {
			return nil, errors.New("invalid UTF-8")
		}
		content = string(data)
	default:
		return nil, errors.New("invalid encoding")
	}

	return map[string]any{"content": content, "encoding": encoding}, nil
}

func writeFile(req request) (any, error) {
	name, err := safePath("/workspace", req.Path, true)
	if err != nil {
		return nil, err
	}
	var data []byte

	switch req.Encoding {
	case "", "utf8":
		if !utf8.ValidString(req.Content) {
			return nil, errors.New("invalid UTF-8")
		}
		data = []byte(req.Content)
	case "base64":
		if strings.ContainsAny(req.Content, "\n\r\t ") {
			return nil, errors.New("invalid base64")
		}
		data, err = base64.StdEncoding.Strict().DecodeString(req.Content)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("invalid encoding")
	}

	if len(data) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	mode := req.Mode
	if mode == "" {
		mode = "create"
	}
	info, statErr := os.Lstat(name)
	if mode == "create" {
		if !os.IsNotExist(statErr) {
			return nil, errors.New("create target exists")
		}
	} else if mode == "replace" {
		if statErr != nil || !info.Mode().IsRegular() || hasMultipleLinks(info) {
			return nil, errors.New("replace requires a regular file")
		}
	} else {
		return nil, errors.New("invalid write mode")
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".knotra-write-")
	if err != nil {
		return nil, err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if mode == "create" {
		err = os.Link(temp, name)
	} else {
		err = os.Rename(temp, name)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"bytesWritten": len(data)}, nil
}

// No untrusted process may survive a command and race subsequent file checks.
func quiesce() error {
	self := os.Getpid()

	for attempt := 0; attempt < 100; attempt++ {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return err
		}
		alive := false

		for _, entry := range entries {
			pid, e := strconv.Atoi(entry.Name())
			if e != nil || pid == 1 || pid == self {
				continue
			}
			stat, e := os.ReadFile("/proc/" + entry.Name() + "/stat")
			if e != nil {
				continue
			}
			close := strings.LastIndexByte(string(stat), ')')
			if close >= 0 && strings.HasPrefix(string(stat[close+1:]), " Z") {
				continue
			}
			alive = true
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}

		if !alive {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}

	return fmt.Errorf("cannot quiesce sandbox processes")
}
