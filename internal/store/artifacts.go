package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/fsutil"
	"github.com/michael-bill/knotra/internal/store/db"
)

const MaxArtifactBytes = 64 << 20

var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Artifacts struct {
	Root  string
	Store *Store
}

func (a Artifacts) Write(name, mediaType string, data []byte, origin map[string]string) (contract.Artifact, error) {
	if len(data) > MaxArtifactBytes {
		return contract.Artifact{}, fmt.Errorf("artifact exceeds 64 MiB")
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	root, e := fsutil.OpenPrivateRoot(a.Root)
	if e != nil {
		return contract.Artifact{}, e
	}
	defer func() { _ = root.Close() }()
	tmp := ".upload-" + uuid.NewString()
	f, e := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return contract.Artifact{}, e
	}
	defer func() { _ = root.Remove(tmp) }()
	if _, e = f.Write(data); e != nil {
		_ = f.Close()
		return contract.Artifact{}, e
	}
	if e = f.Sync(); e != nil {
		_ = f.Close()
		return contract.Artifact{}, e
	}
	if e = f.Close(); e != nil {
		return contract.Artifact{}, e
	}
	// A shared writer must never replace content that is already published.
	if e = root.Link(tmp, hash); e != nil {
		if !errors.Is(e, os.ErrExist) {
			return contract.Artifact{}, e
		}
		if _, e = readArtifactFile(root, contract.Artifact{SHA256: hash, Size: int64(len(data))}); e != nil {
			return contract.Artifact{}, e
		}
	}
	dir, e := root.Open(".")
	if e != nil {
		return contract.Artifact{}, e
	}
	e = fsutil.SyncOpenDir(dir)
	closeErr := dir.Close()
	if e != nil {
		return contract.Artifact{}, e
	}
	if closeErr != nil {
		return contract.Artifact{}, closeErr
	}
	if origin == nil {
		origin = map[string]string{}
	}
	return contract.Artifact{
		ID:        uuid.NewString(),
		Name:      name,
		MediaType: mediaType,
		Size:      int64(len(data)),
		SHA256:    hash,
		Origin:    origin,
	}, nil
}

func RegisterArtifact(ctx context.Context, tx pgx.Tx, art contract.Artifact) error {
	b, e := raw(art)
	if e != nil {
		return e
	}
	_, e = db.New(tx).RegisterArtifact(ctx, db.RegisterArtifactParams{ID: art.ID, Document: b})
	return e
}

func (a Artifacts) Put(ctx context.Context, name, mediaType string, data []byte, origin map[string]string) (contract.Artifact, error) {
	v, e := a.Write(name, mediaType, data, origin)
	if e != nil {
		return v, e
	}
	b, e := raw(v)
	if e != nil {
		return v, e
	}
	_, e = db.New(a.Store.Pool).InsertArtifact(ctx, db.InsertArtifactParams{ID: v.ID, Document: b})
	return v, e
}

func ReadArtifact(ctx context.Context, q Querier, id string) (contract.Artifact, error) {
	var v contract.Artifact
	var b []byte
	b, e := db.New(q).ReadArtifact(ctx, id)
	if e == nil {
		e = json.Unmarshal(b, &v)
	}
	return v, classify(e)
}

func (s *Store) Artifacts(ctx context.Context, cursor string) ([]contract.Artifact, error) {
	documents, err := db.New(s.Pool).ListArtifacts(ctx, cursor)
	return decodeRecords[contract.Artifact](documents, err)
}

func (a Artifacts) Get(ctx context.Context, id string) ([]byte, error) {
	var v contract.Artifact
	var meta []byte
	meta, e := db.New(a.Store.Pool).ReadArtifactMetadata(ctx, id)
	if e == nil {
		e = json.Unmarshal(meta, &v)
	}
	if e != nil {
		return nil, e
	}
	root, e := os.OpenRoot(a.Root)
	if e != nil {
		return nil, e
	}
	defer func() { _ = root.Close() }()
	return readArtifactFile(root, v)
}

func readArtifactFile(root *os.Root, v contract.Artifact) ([]byte, error) {
	if !hashPattern.MatchString(v.SHA256) {
		return nil, fmt.Errorf("invalid artifact checksum")
	}
	fi, e := root.Lstat(v.SHA256)
	if e != nil {
		return nil, e
	}
	if !fi.Mode().IsRegular() || fi.Size() != v.Size || fi.Size() > MaxArtifactBytes {
		return nil, fmt.Errorf("artifact size or file type changed")
	}
	f, e := root.Open(v.SHA256)
	if e != nil {
		return nil, e
	}
	defer func() { _ = f.Close() }()
	opened, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !os.SameFile(fi, opened) {
		return nil, fmt.Errorf("artifact changed while opening")
	}
	b, e := io.ReadAll(io.LimitReader(f, MaxArtifactBytes+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) != v.Size {
		return nil, fmt.Errorf("artifact size changed")
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != v.SHA256 {
		return nil, fmt.Errorf("artifact checksum mismatch")
	}
	return b, nil
}

func (s *Store) Artifact(ctx context.Context, id string) (contract.Artifact, error) {
	return ReadArtifact(ctx, s.Pool, id)
}

// ArtifactValue resolves opaque IDs against the engine registry. Wire callers
// cannot forge a filesystem path, checksum or origin by supplying a descriptor.
func ArtifactValue(ctx context.Context, q Querier, b json.RawMessage) (contract.Value, error) {
	var id string
	var ids []string
	collection := false
	switch {
	case json.Unmarshal(b, &id) == nil && id != "":
		ids = []string{id}
	case json.Unmarshal(b, &ids) == nil && ids != nil:
		collection = true
	default:
		return contract.Value{}, &ValidationError{"artifact must be a registered ID or array of IDs"}

	}
	value := contract.Value{Collection: collection, Artifacts: []contract.Artifact{}}

	for _, id := range ids {
		a, err := ReadArtifact(ctx, q, id)
		if err != nil {
			return contract.Value{}, err
		}
		value.Artifacts = append(value.Artifacts, a)
	}
	return value, nil
}
