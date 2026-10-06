-- name: ReserveNodeInstances :execresult
UPDATE knotra_execution_scopes SET materialized_instances=materialized_instances+$3
				WHERE run_id=$1 AND id=$2 AND materialized_instances+$3<=(limits->>'maxNodeInstances')::integer;

-- name: InsertNodeDeadline :execresult
INSERT INTO knotra_execution_timers(run_id,id,generation,instance_id,kind,due_at)
				VALUES($1,$2,1,$3,'node_deadline',$4) ON CONFLICT DO NOTHING;

-- name: InsertExecutionTimer :execresult
INSERT INTO knotra_execution_timers(run_id,id,generation,instance_id,kind,due_at)
			VALUES($1,$2,$3,$4,$5,$6);

-- name: PauseRunAdmission :execresult
UPDATE knotra_runs SET admission_paused=true WHERE id=$1;

-- name: ResumeRunAdmission :execresult
UPDATE knotra_runs SET admission_paused=false WHERE id=$1;

-- name: SetRunStopCause :execresult
UPDATE knotra_runs SET stop_cause=$2 WHERE id=$1;

-- name: ClearAttemptDispatch :execresult
UPDATE knotra_execution_attempts SET dispatch_pending=false
			WHERE run_id=$1 AND instance_id=$2 AND number=$3;

-- name: SetAttemptDispatchGeneration :execresult
UPDATE knotra_execution_attempts SET dispatch_generation=$4,dispatch_pending=false
			WHERE run_id=$1 AND instance_id=$2 AND number=$3;

-- name: ListDueTimerRuns :many
SELECT DISTINCT run_id FROM knotra_execution_timers
		WHERE consumed_at IS NULL AND due_at<=clock_timestamp() ORDER BY run_id LIMIT 64;

-- name: ConsumeDueTimers :execresult
WITH due AS (
    SELECT id,generation FROM knotra_execution_timers
    WHERE run_id=$1 AND consumed_at IS NULL AND due_at<=$2
    ORDER BY due_at,id,generation LIMIT 64 FOR UPDATE SKIP LOCKED
)
UPDATE knotra_execution_timers t SET consumed_at=$2 FROM due
WHERE t.run_id=$1 AND t.id=due.id AND t.generation=due.generation;

-- name: ListExpiredOutcomeAttempts :many
-- Fenced attempts can receive an envelope after their unknown/stop decision.
-- Import evidence without reopening their already decided graph transition.
SELECT run_id,instance_id,number,outcome_key,owner,ownership_generation,state FROM knotra_execution_attempts
		WHERE outcome IS NULL AND owner IS NOT NULL AND lease_expires_at<=clock_timestamp()
		AND outcome_key>$1 ORDER BY outcome_key LIMIT 64;

-- name: MaximumActiveExecutionDurationMicros :one
SELECT COALESCE(max((extract(epoch FROM (execution_deadline-admitted_at))*1000000)::bigint),0)::bigint
FROM knotra_runs WHERE backend='river' AND document->>'status' NOT IN ('succeeded','failed','cancelled','skipped');

-- name: RequiredExecutionBackends :many
-- Retained River state may still require outcome/resource cleanup after a run
-- terminates. Temporal is needed only for live runs or undelivered commands.
SELECT DISTINCT backend FROM knotra_runs
WHERE backend='river' OR document->>'status' NOT IN ('succeeded','failed','cancelled','skipped')
UNION
SELECT 'temporal'::text WHERE EXISTS(SELECT 1 FROM knotra_outbox WHERE sent_at IS NULL)
ORDER BY backend;
