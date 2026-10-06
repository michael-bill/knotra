-- name: RegisterArtifact :execresult
INSERT INTO knotra_artifacts(id,document,published) VALUES($1,$2,true);

-- name: InsertArtifact :execresult
INSERT INTO knotra_artifacts(id,document) VALUES($1,$2);

-- name: ReadArtifact :one
SELECT document FROM knotra_artifacts WHERE id=$1 AND published;

-- name: ReadArtifactMetadata :one
SELECT document FROM knotra_artifacts WHERE id=$1;

-- name: ListArtifacts :many
SELECT document FROM knotra_artifacts WHERE published AND ($1::text='' OR id<$1) ORDER BY id DESC LIMIT 101;
