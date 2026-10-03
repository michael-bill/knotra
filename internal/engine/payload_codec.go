package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/proto"
)

const (
	externalPayloadEncoding = "binary/knotra.external.v1"
	// A workflow task can dispatch many activities. Keep enough space below the
	// transaction limit for all arguments, results, timer commands and metadata.
	defaultPayloadThreshold = 8 * 1024
)

// BlobStore keeps content-addressed Temporal payloads for at least the lifetime
// of their histories. Put must durably store bytes before returning their SHA256
// key; Get must return the exact original bytes. Workers and API processes must
// share this store. It is called by the SDK converter, never by workflow code.
type BlobStore interface {
	Put([]byte) (string, error)
	Get(string) ([]byte, error)
}

// PayloadCodec moves large protobuf Payloads out of Temporal history, retaining
// every metadata field. Small payloads retain their normal SDK representation.
// External references are hash checked even when BlobStore is not a filesystem.
type PayloadCodec struct {
	Store     BlobStore
	Threshold int
}

var _ converter.PayloadCodec = (*PayloadCodec)(nil)

func (c *PayloadCodec) Encode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	if c.Store == nil {
		return nil, errors.New("payload blob store is required")
	}
	threshold := c.Threshold
	if threshold <= 0 {
		threshold = defaultPayloadThreshold
	}
	result := make([]*commonpb.Payload, len(payloads))

	for index, payload := range payloads {
		if payload == nil || string(payload.Metadata[converter.MetadataEncoding]) == externalPayloadEncoding || proto.Size(payload) < threshold {
			result[index] = payload
			continue
		}
		data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal payload: %w", err)
		}
		key, err := c.Store.Put(data)
		if err != nil {
			return nil, fmt.Errorf("store payload: %w", err)
		}
		if key != blobKey(data) {
			return nil, errors.New("blob store returned an invalid content key")
		}
		result[index] = &commonpb.Payload{
			Metadata: map[string][]byte{converter.MetadataEncoding: []byte(externalPayloadEncoding)},
			Data:     []byte(key),
		}
	}

	return result, nil
}

func (c *PayloadCodec) Decode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	if c.Store == nil {
		return nil, errors.New("payload blob store is required")
	}
	result := make([]*commonpb.Payload, len(payloads))

	for index, payload := range payloads {
		if payload == nil || string(payload.Metadata[converter.MetadataEncoding]) != externalPayloadEncoding {
			result[index] = payload
			continue
		}
		key := string(payload.Data)
		if !validBlobKey(key) {
			return nil, errors.New("invalid external payload key")
		}
		data, err := c.Store.Get(key)
		if err != nil {
			return nil, fmt.Errorf("read payload %s: %w", key, err)
		}
		if blobKey(data) != key {
			return nil, fmt.Errorf("payload %s failed SHA256 verification", key)
		}
		decoded := &commonpb.Payload{}
		if err := proto.Unmarshal(data, decoded); err != nil {
			return nil, fmt.Errorf("decode stored payload: %w", err)
		}
		if string(decoded.Metadata[converter.MetadataEncoding]) == externalPayloadEncoding {
			return nil, errors.New("nested external payload references are not permitted")
		}
		result[index] = decoded
	}

	return result, nil
}

func blobKey(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }

func validBlobKey(key string) bool {
	if len(key) != sha256.Size*2 {
		return false
	}

	for _, char := range key {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}

	return true
}

// FileBlobStore is immutable local storage for a single-host deployment. The
// directory must live on persistent storage and be shared by all local workers.
// Keeping histories while deleting this directory makes replay impossible.
type FileBlobStore struct{ directory string }

func NewFileBlobStore(directory string) (*FileBlobStore, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve payload directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, fmt.Errorf("create payload directory: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("payload directory must be a real directory")
	}
	if err := os.Chmod(absolute, 0700); err != nil {
		return nil, err
	}
	return &FileBlobStore{directory: absolute}, nil
}

func (s *FileBlobStore) Put(data []byte) (string, error) {
	key := blobKey(data)
	if _, err := s.Get(key); err == nil {
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	file, err := os.CreateTemp(s.directory, ".payload-")
	if err != nil {
		return "", err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err := file.Write(data); err != nil {
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
	// Link publishes a fully written inode without replacing an existing key.
	// Concurrent writers converge on the winner, whose bytes are verified.
	if err := os.Link(temporary, filepath.Join(s.directory, key)); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if _, err := s.Get(key); err != nil {
			return "", err
		}
	}
	if err := os.Remove(temporary); err != nil {
		return "", err
	}
	directory, err := os.Open(s.directory)
	if err != nil {
		return "", err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return "", err
	}
	return key, nil
}

func (s *FileBlobStore) Get(key string) ([]byte, error) {
	if !validBlobKey(key) {
		return nil, errors.New("invalid payload key")
	}
	path := filepath.Join(s.directory, key)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("payload must be a regular file, not a link")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errors.New("payload changed while opening")
	}
	// This is an engine-owned immutable file, not a client-supplied length. Bound
	// the read to its observed size so concurrent growth cannot produce an
	// unbounded stream. Checkpoints may aggregate many valid large node outputs;
	// imposing an unrelated codec cap would make their workflow tasks fail forever.
	if after.Size() < 0 || after.Size() == math.MaxInt64 {
		return nil, errors.New("payload size cannot be addressed")
	}
	data, err := io.ReadAll(io.LimitReader(file, after.Size()+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != after.Size() {
		return nil, errors.New("payload size changed while reading")
	}
	if blobKey(data) != key {
		return nil, errors.New("stored payload failed SHA256 verification")
	}
	return data, nil
}
