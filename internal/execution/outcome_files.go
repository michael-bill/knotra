package execution

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/michael-bill/knotra/internal/fsutil"
)

var ErrInvalidOutcome = errors.New("invalid durable outcome")

var ErrOutcomeConflict = errors.New("outcome already exists with different evidence")

type outcomeFile struct {
	SHA256 string          `json:"sha256"`
	Data   json.RawMessage `json:"data"`
}

// OutcomeFiles holds execution evidence on local or suitable shared filesystems. Outcomes and initialization
// identities are immutable; a serialized MCP session replaces its current-call record.
// Keep this directory with the database backup, including uncommitted outcomes
// and MCP IDs.
// The root handle confines access even if a directory is renamed concurrently.
type OutcomeFiles struct{ root *os.Root }

func OpenOutcomeFiles(directory string) (*OutcomeFiles, error) {
	root, err := fsutil.OpenPrivateRoot(directory)
	if err != nil {
		return nil, err
	}
	return &OutcomeFiles{root: root}, nil
}

func (s *OutcomeFiles) Close() error { return s.root.Close() }

// SavedAt reads immutable envelope metadata without loading potentially large
// execution outputs. It uses the same confined key and regular-file checks as Get.
func (s *OutcomeFiles) SavedAt(key string) (time.Time, error) {
	if !validKey(key) {
		return time.Time{}, fmt.Errorf("%w: invalid outcome key", ErrInvalidOutcome)
	}
	info, err := s.root.Lstat(key + ".json")
	if err != nil {
		return time.Time{}, err
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, fmt.Errorf("%w: outcome must be a regular file", ErrInvalidOutcome)
	}
	return info.ModTime(), nil
}

// Put returns after syncing file contents and supported directory metadata. Link publishes
// without replacing an existing result, including concurrent writers. Identical
// delivery is idempotent; conflicting evidence is retained as an explicit error.
func (s *OutcomeFiles) Put(outcome Outcome) (string, error) {
	if err := outcome.Validate(); err != nil {
		return "", err
	}
	key, err := outcome.Ownership.OutcomeKey()
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(outcome)
	if err != nil {
		return "", err
	}
	return s.put(key, data)
}

func (s *OutcomeFiles) put(key string, data []byte) (string, error) {
	return s.publish(key, data, false)
}

func (s *OutcomeFiles) publish(key string, data []byte, replace bool) (string, error) {
	sum := sha256.Sum256(data)
	encoded, err := json.Marshal(outcomeFile{SHA256: hex.EncodeToString(sum[:]), Data: data})
	if err != nil {
		return "", err
	}
	temporary := ".outcome-" + uuid.NewString()
	file, err := s.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer func() { _ = s.root.Remove(temporary) }()
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if replace {
		if err := s.root.Rename(temporary, key+".json"); err != nil {
			return "", err
		}
	} else if err := s.root.Link(temporary, key+".json"); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		previous, err := s.read(key)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(previous, data) {
			return "", ErrOutcomeConflict
		}
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return "", err
	}
	err = fsutil.SyncOpenDir(dir)
	closeErr := dir.Close()
	if err != nil {
		return "", err
	}
	return key, closeErr
}

func validKey(key string) bool {
	if len(key) != sha256.Size*2 {
		return false
	}
	for _, c := range key {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func decodeStrict(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing outcome data")
	}
	return nil
}

func (s *OutcomeFiles) read(key string) ([]byte, error) {
	return s.readBounded(key, 0)
}

func (s *OutcomeFiles) readBounded(key string, limit int64) ([]byte, error) {
	if !validKey(key) {
		return nil, fmt.Errorf("%w: invalid outcome key", ErrInvalidOutcome)
	}
	name := key + ".json"
	info, err := s.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: outcome must be a regular file", ErrInvalidOutcome)
	}
	file, err := s.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	// These are executor-produced durable records. An arbitrary byte cap here
	// would make a contract-valid large output impossible to recover.
	var reader io.Reader = file
	if limit > 0 {
		reader = io.LimitReader(file, limit+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: evidence exceeds byte limit", ErrInvalidOutcome)
	}
	var envelope outcomeFile
	if err := decodeStrict(data, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decode envelope: %w", ErrInvalidOutcome, err)
	}
	sum := sha256.Sum256(envelope.Data)
	if hex.EncodeToString(sum[:]) != envelope.SHA256 {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrInvalidOutcome)
	}
	return envelope.Data, nil
}

func (s *OutcomeFiles) Get(key string) (Outcome, error) {
	var outcome Outcome
	data, err := s.read(key)
	if err != nil {
		return outcome, err
	}
	if err := decodeStrict(data, &outcome); err != nil {
		return outcome, errors.Join(ErrInvalidOutcome, err)
	}
	if err := outcome.Validate(); err != nil {
		return outcome, errors.Join(ErrInvalidOutcome, err)
	}
	expected, err := outcome.Ownership.OutcomeKey()
	if err != nil || expected != key {
		return outcome, fmt.Errorf("%w: ownership does not match storage key", ErrInvalidOutcome)
	}
	return outcome, nil
}
