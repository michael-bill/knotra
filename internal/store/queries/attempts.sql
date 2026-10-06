-- name: RegisterWorker :execresult
INSERT INTO knotra_workers(id,engine_id,host_id,scheduler_versions,state_format_versions,capabilities)
		VALUES(sqlc.arg(id),sqlc.arg(engine_id),sqlc.arg(host_id),ARRAY[sqlc.arg(column4)::integer]::integer[],ARRAY[sqlc.arg(column5)::integer]::integer[],sqlc.arg(capabilities)::jsonb) ON CONFLICT DO NOTHING;

-- name: LockWorkerHeartbeat :one
SELECT heartbeat_at FROM knotra_workers WHERE id=$1 AND engine_id=$2 FOR UPDATE;

-- name: DatabaseTime :one
SELECT clock_timestamp()::timestamptz;

-- name: UpdateWorkerHeartbeat :execresult
UPDATE knotra_workers SET heartbeat_at=$2 WHERE id=$1;

-- name: DrainWorker :execresult
UPDATE knotra_workers SET draining=true WHERE id=$1 AND engine_id=$2;

-- name: MarkPausedAttemptForDispatch :execresult
UPDATE knotra_execution_attempts SET dispatch_pending=true
			WHERE run_id=$1 AND instance_id=$2 AND number=$3 AND state='ready' AND dispatch_generation=$4;

-- name: LockClaimNode :one
SELECT n.state AS node_state,n.attempt_number,n.execution_request,n.deadline,g.state AS graph_state,g.scopes,
		n.node_id,g.pipeline,g.graph_path,g.parent_instance_id,g.iteration_index,n.started_at,(r.plan->>'digest')::text AS plan_digest
		FROM knotra_execution_nodes n JOIN knotra_execution_graphs g ON g.run_id=n.run_id AND g.id=n.graph_id
		JOIN knotra_runs r ON r.id=n.run_id
		WHERE n.run_id=$1 AND n.id=$2 FOR UPDATE OF n;

-- name: LockClaimAttempt :one
SELECT state,dispatch_generation,ownership_generation FROM knotra_execution_attempts
		WHERE run_id=$1 AND instance_id=$2 AND number=$3 FOR UPDATE;

-- name: ReadClaimWorker :one
SELECT heartbeat_at,draining,(sqlc.arg(scheduler_version)::integer=ANY(scheduler_versions) AND sqlc.arg(state_format_version)::integer=ANY(state_format_versions)
    AND COALESCE(capabilities->>'role','all') IN ('all','executor')) AS compatible
		FROM knotra_workers WHERE id=sqlc.arg(id) AND engine_id=sqlc.arg(engine_id) FOR SHARE;

-- name: LockClaimScopes :many
SELECT id,limits,active_attempts FROM knotra_execution_scopes
		WHERE run_id=sqlc.arg(run_id) AND id=ANY(sqlc.arg(scope_i_ds)::text[]) ORDER BY id FOR UPDATE;

-- name: MarkAttemptForDispatch :execresult
UPDATE knotra_execution_attempts SET dispatch_pending=true
			WHERE run_id=$1 AND instance_id=$2 AND number=$3;

-- name: ReserveAttemptSlot :execresult
INSERT INTO knotra_execution_slots(run_id,instance_id,attempt_number,scope_id,ownership_generation)
			VALUES($1,$2,$3,$4,$5);

-- name: IncreaseActiveAttempts :execresult
UPDATE knotra_execution_scopes SET active_attempts=active_attempts+1 WHERE run_id=$1 AND id=$2;

-- name: SetAttemptClaimed :execresult
UPDATE knotra_execution_attempts SET state='claimed',dispatch_pending=false,owner=$4,
		ownership_generation=$5,lease_expires_at=$6,outcome_key=$7 WHERE run_id=$1 AND instance_id=$2 AND number=$3;

-- name: SetNodeRunning :execresult
UPDATE knotra_execution_nodes SET state='running',started_at=$3,revision=revision+1 WHERE run_id=$1 AND id=$2;

-- name: IncrementRunRevision :execresult
UPDATE knotra_runs SET state_revision=state_revision+1 WHERE id=$1;

-- name: LockOwnedNode :one
SELECT state,attempt_number,deadline FROM knotra_execution_nodes WHERE run_id=$1 AND id=$2 FOR UPDATE;

-- name: LockAttemptLease :one
SELECT lease_expires_at FROM knotra_execution_attempts
		WHERE run_id=$1 AND instance_id=$2 AND number=$3 AND state='claimed' AND owner=$4 AND ownership_generation=$5 FOR UPDATE;

-- name: ReadOwnedWorkerHeartbeat :one
SELECT heartbeat_at FROM knotra_workers WHERE id=sqlc.arg(id) AND engine_id=sqlc.arg(engine_id)
		AND sqlc.arg(scheduler_version)::integer=ANY(scheduler_versions) AND sqlc.arg(state_format_version)::integer=ANY(state_format_versions) FOR SHARE;

-- name: RenewAttempt :one
UPDATE knotra_execution_attempts SET lease_expires_at=LEAST(sqlc.arg(lease_expires_at),sqlc.arg(root_deadline),
		(SELECT n.deadline FROM knotra_execution_nodes n WHERE n.run_id=sqlc.arg(run_id) AND n.id=sqlc.arg(id)))
		WHERE run_id=sqlc.arg(run_id) AND instance_id=sqlc.arg(id) AND number=sqlc.arg(number) RETURNING lease_expires_at;

-- name: LockCompletedAttempt :one
SELECT state FROM knotra_execution_attempts WHERE run_id=$1 AND instance_id=$2 AND number=$3
		AND owner=$4 AND ownership_generation=$5 FOR UPDATE;

-- name: LockReservedScopeIDs :many
SELECT s.id FROM knotra_execution_scopes s JOIN knotra_execution_slots r
		ON r.run_id=s.run_id AND r.scope_id=s.id WHERE r.run_id=$1 AND r.instance_id=$2 AND r.attempt_number=$3
		AND r.ownership_generation=$4 AND r.released_at IS NULL ORDER BY s.id FOR UPDATE OF s;

-- name: DecreaseActiveAttempts :execresult
UPDATE knotra_execution_scopes SET active_attempts=active_attempts-1 WHERE run_id=$1 AND id=$2;

-- name: ReleaseReservedSlots :execresult
UPDATE knotra_execution_slots SET released_at=clock_timestamp()
		WHERE run_id=$1 AND instance_id=$2 AND attempt_number=$3 AND ownership_generation=$4 AND released_at IS NULL;
