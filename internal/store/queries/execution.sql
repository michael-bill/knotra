-- name: SetRiverAdmission :execresult
UPDATE knotra_runs SET backend='river',scheduler_version=$2,state_format_version=$3,
		admitted_at=$4,execution_deadline=$5,wake_generation=1,dirty_since=$4 WHERE id=$1;

-- name: InsertRootScope :execresult
INSERT INTO knotra_execution_scopes(run_id,id,limits) VALUES($1,$1,$2);

-- name: InsertRootDeadline :execresult
INSERT INTO knotra_execution_timers(run_id,id,generation,kind,due_at) VALUES($1,'root_deadline',1,'run_deadline',$2);

-- name: LockExecutionRun :one
-- Materialize the locked row before evaluating the database clock. Sampling
-- in the inner SELECT would happen before a contended FOR UPDATE finishes.
WITH locked AS MATERIALIZED (
SELECT backend,scheduler_version,state_format_version,admitted_at,execution_deadline,
		state_revision,wake_generation,applied_generation,cancel_requested,admission_paused,stop_cause,(document->>'status')::text AS status,graph_cursor,graph_scan_again,delivery_cursor
		FROM knotra_runs WHERE id=$1 FOR UPDATE
)
SELECT locked.*,clock_timestamp()::timestamptz AS now FROM locked;

-- name: WakeExecution :one
UPDATE knotra_runs SET wake_generation=wake_generation+1,state_revision=state_revision+1,
    dirty_since=COALESCE(dirty_since,clock_timestamp()) WHERE id=$1 RETURNING wake_generation;

-- name: ApplyWake :execresult
UPDATE knotra_runs SET applied_generation=wake_generation,state_revision=state_revision+1,dirty_since=NULL WHERE id=$1;

-- name: NextExecutionEventSequence :one
UPDATE knotra_runs SET execution_event_sequence=execution_event_sequence+1,state_revision=state_revision+1
		WHERE id=$1 RETURNING execution_event_sequence;
