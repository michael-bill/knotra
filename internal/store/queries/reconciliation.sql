-- name: LockLostAttempt :one
SELECT a.state,a.owner,a.ownership_generation,a.lease_expires_at,a.outcome_key,a.outcome,
    n.state AS node_state,n.attempt_number,n.execution_request
FROM knotra_execution_nodes n JOIN knotra_execution_attempts a
    ON a.run_id=n.run_id AND a.instance_id=n.id
WHERE a.run_id=$1 AND a.instance_id=$2 AND a.number=$3
FOR UPDATE OF n,a;

-- name: FirstUnconfirmedOperation :one
SELECT id FROM knotra_operations WHERE run_id=$1 AND instance_id=$2 AND attempt_number=$3
    AND admitted_at IS NOT NULL AND NOT completed
ORDER BY created_at,id LIMIT 1;

-- name: SetLostAttempt :execresult
UPDATE knotra_execution_attempts SET state=sqlc.arg(state),result=sqlc.arg(result),failure=sqlc.arg(failure)
WHERE run_id=sqlc.arg(run_id) AND instance_id=sqlc.arg(instance_id) AND number=sqlc.arg(number)
    AND state='claimed';

-- name: ListDeliveryRepairRuns :many
SELECT r.id FROM knotra_runs r
WHERE r.backend='river' AND r.id>sqlc.arg(cursor)::text
    AND r.document->>'status' NOT IN ('succeeded','failed','cancelled')
    AND (r.wake_generation>r.applied_generation OR EXISTS(
        SELECT 1 FROM knotra_execution_attempts a WHERE a.run_id=r.id AND a.state='ready'))
ORDER BY r.id LIMIT 64;

-- name: ReadReadyDeliveries :many
SELECT a.instance_id,a.number,a.dispatch_generation
FROM knotra_execution_attempts a JOIN knotra_execution_nodes n ON n.run_id=a.run_id AND n.id=a.instance_id
WHERE a.run_id=sqlc.arg(run_id) AND a.state='ready' AND n.state='ready' AND a.number=n.attempt_number
    AND a.instance_id>sqlc.arg(cursor)::text AND n.deadline>sqlc.arg(now)::timestamptz
    AND NOT EXISTS(
        SELECT 1 FROM jsonb_array_elements(n.execution_request->'scopes') scope
        LEFT JOIN knotra_execution_scopes s ON s.run_id=a.run_id AND s.id=scope->>'id'
        WHERE s.id IS NULL OR s.active_attempts>=(s.limits->>'maxConcurrentNodes')::integer
    )
ORDER BY a.instance_id LIMIT 64;

-- name: SetDeliveryCursor :execresult
UPDATE knotra_runs SET delivery_cursor=$2,state_revision=state_revision+1 WHERE id=$1;

-- name: BindWorkerDeliveryClient :execresult
UPDATE knotra_workers SET delivery_client_id=$3
WHERE id=$1 AND engine_id=$2 AND (delivery_client_id IS NULL OR delivery_client_id=$3);

-- name: ReadDeliveryWorkerHeartbeat :one
SELECT heartbeat_at FROM knotra_workers WHERE engine_id=$1 AND delivery_client_id=$2;
