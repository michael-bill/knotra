-- name: LockRunCommand :one
SELECT document,cancel_requested FROM knotra_runs WHERE id=$1 FOR UPDATE;

-- name: SetRunCancelled :execresult
UPDATE knotra_runs SET cancel_requested=true WHERE id=$1;

-- name: ReadRequestRunID :one
SELECT run_id FROM knotra_requests WHERE id=$1;

-- name: LockRequest :one
SELECT document,status FROM knotra_requests WHERE id=$1 FOR UPDATE;

-- name: AcceptHumanAnswer :execresult
UPDATE knotra_requests SET status='answered',response_id=$2,response=$3,accepted_at=$4 WHERE id=$1;

-- name: LockResolutionRequest :one
SELECT document,status FROM knotra_requests WHERE run_id=sqlc.arg(run_id) AND kind='resolution' AND document->>'instanceId'=sqlc.arg(instance_id)::text ORDER BY created_at DESC LIMIT 1 FOR UPDATE;

-- name: AcceptResolution :execresult
UPDATE knotra_requests SET status='resolved',response_id=$2,response=$3,accepted_at=$4 WHERE id=$1;

-- name: LockResolutionAnswer :one
SELECT status,response,accepted_at FROM knotra_requests WHERE id=$1 AND run_id=$2 AND kind='resolution' FOR UPDATE;
