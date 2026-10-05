package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/michael-bill/knotra/internal/fsutil"
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
	if e := os.MkdirAll(a.Root, 0700); e != nil {
		return contract.Artifact{}, e
	}
	f, e := os.CreateTemp(a.Root, ".upload-")
	if e != nil {
		return contract.Artifact{}, e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(data); e != nil {
		f.Close()
		return contract.Artifact{}, e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return contract.Artifact{}, e
	}
	if e = f.Close(); e != nil {
		return contract.Artifact{}, e
	}
	if e = os.Rename(tmp, filepath.Join(a.Root, hash)); e != nil {
		return contract.Artifact{}, e
	}
	e = fsutil.SyncDir(a.Root)
	if e != nil {
		return contract.Artifact{}, e
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
	_, e = tx.Exec(ctx, "INSERT INTO knotra_artifacts(id,document,published) VALUES($1,$2,true)", art.ID, b)
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
	_, e = a.Store.Pool.Exec(ctx, "INSERT INTO knotra_artifacts(id,document) VALUES($1,$2)", v.ID, b)
	return v, e
}

func ReadArtifact(ctx context.Context, q Querier, id string) (contract.Artifact, error) {
	var v contract.Artifact
	var b []byte
	e := q.QueryRow(ctx, "SELECT document FROM knotra_artifacts WHERE id=$1 AND published", id).Scan(&b)
	if e == nil {
		e = json.Unmarshal(b, &v)
	}
	return v, classify(e)
}

func (s *Store) Artifacts(ctx context.Context, cursor string) ([]contract.Artifact, error) {
	return list[contract.Artifact](
		ctx,
		s.Pool,
		"SELECT document FROM knotra_artifacts WHERE published AND ($1='' OR id<$1) ORDER BY id DESC LIMIT 101",
		cursor,
	)
}

func (a Artifacts) Get(ctx context.Context, id string) ([]byte, error) {
	var v contract.Artifact
	var meta []byte
	e := a.Store.Pool.QueryRow(ctx, "SELECT document FROM knotra_artifacts WHERE id=$1", id).Scan(&meta)
	if e == nil {
		e = json.Unmarshal(meta, &v)
	}
	if e != nil {
		return nil, e
	}
	if !hashPattern.MatchString(v.SHA256) {
		return nil, fmt.Errorf("invalid artifact checksum")
	}
	root, e := os.OpenRoot(a.Root)
	if e != nil {
		return nil, e
	}
	defer root.Close()
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
	defer f.Close()
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
	if json.Unmarshal(b, &id) == nil && id != "" {
		ids = []string{id}
	} else if json.Unmarshal(b, &ids) == nil && ids != nil {
		collection = true
	} else {
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
