-- name: ReadResourceHost :one
SELECT host_id FROM knotra_workers WHERE id=$1 AND engine_id=$2;

-- name: InsertOwnedResource :execresult
INSERT INTO knotra_resources(id,engine_id,host_id,run_id,instance_id,attempt_number,worker_id,ownership_generation,kind,lifetime)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10);

-- name: CloseOwnedResource :execresult
UPDATE knotra_resources SET state='closed',closed_at=COALESCE(closed_at,clock_timestamp())
WHERE id=$1 AND engine_id=$2 AND run_id=$3 AND instance_id=$4 AND attempt_number=$5
    AND worker_id=$6 AND ownership_generation=$7;

-- name: ReadResourceIdentity :one
SELECT run_id FROM knotra_resources WHERE id=$1 AND engine_id=$2;

-- name: LockResourceCleanup :one
SELECT r.engine_id,r.host_id,r.run_id,r.instance_id,r.attempt_number,r.worker_id,
    r.ownership_generation,r.cleanup_generation,r.kind,r.lifetime,r.state,r.closed_at,w.heartbeat_at,a.state AS attempt_state,
    a.lease_expires_at,r.mcp_session
FROM knotra_resources r JOIN knotra_workers w ON w.id=r.worker_id
JOIN knotra_execution_attempts a ON a.run_id=r.run_id AND a.instance_id=r.instance_id AND a.number=r.attempt_number
WHERE r.id=$1 AND r.engine_id=$2 FOR UPDATE OF r,w;

-- name: RecordOwnedMCPSession :execresult
UPDATE knotra_resources SET mcp_session=sqlc.arg(mcp_session)::jsonb
WHERE id=$1 AND engine_id=$2 AND run_id=$3 AND instance_id=$4 AND attempt_number=$5
    AND worker_id=$6 AND ownership_generation=$7 AND kind='mcp_http'
    AND (mcp_session IS NULL OR mcp_session=sqlc.arg(mcp_session)::jsonb);

-- name: ClaimResourceCleanup :execresult
UPDATE knotra_resources SET state='cleaning' WHERE id=$1;

-- name: ReadResourceCleanupCandidates :many
SELECT id,worker_id,ownership_generation
FROM knotra_resources
WHERE engine_id=$1 AND host_id=$2 AND state<>'closed' AND id>sqlc.arg(cursor)
ORDER BY id LIMIT 64;

-- name: BumpResourceCleanupDelivery :one
UPDATE knotra_resources SET cleanup_generation=cleanup_generation+1
WHERE id=$1 AND state='cleaning' RETURNING cleanup_generation;

-- name: CloseCleanedResource :execresult
UPDATE knotra_resources SET state='closed',closed_at=COALESCE(closed_at,clock_timestamp())
WHERE id=$1 AND engine_id=$2 AND host_id=$3 AND worker_id=$4
    AND ownership_generation=$5 AND cleanup_generation=$6 AND state='cleaning';
