-- name: LockOutcomeAttempt :one
SELECT n.state AS node_state,n.attempt_number,n.deadline,a.state AS attempt_state,a.owner,a.ownership_generation,a.outcome_key,a.outcome,(r.plan->>'digest')::text AS plan_digest
		FROM knotra_execution_nodes n JOIN knotra_execution_attempts a ON a.run_id=n.run_id AND a.instance_id=n.id
		JOIN knotra_runs r ON r.id=n.run_id WHERE n.run_id=$1 AND n.id=$2 AND a.number=$3 FOR UPDATE OF n,a;

-- name: OutcomeMatches :one
SELECT outcome=sqlc.arg(outcome)::jsonb FROM knotra_execution_attempts
			WHERE run_id=sqlc.arg(run_id) AND instance_id=sqlc.arg(instance_id) AND number=sqlc.arg(number);

-- name: SaveOutcomeEvidence :execresult
UPDATE knotra_execution_attempts SET outcome=$4,evidence_key=$5
		WHERE run_id=$1 AND instance_id=$2 AND number=$3;

-- name: CancelAttempt :execresult
UPDATE knotra_execution_attempts SET state='cancelled'
				WHERE run_id=$1 AND instance_id=$2 AND number=$3;

-- name: HasUnconfirmedOperations :one
SELECT EXISTS(SELECT 1 FROM knotra_operations WHERE run_id=$1 AND instance_id=$2
			AND attempt_number=$3 AND kind IN ('model','tool') AND admitted_at IS NOT NULL AND NOT completed);

-- name: SetAttemptCompleted :execresult
UPDATE knotra_execution_attempts SET state='completed',result=$4
		WHERE run_id=$1 AND instance_id=$2 AND number=$3;
