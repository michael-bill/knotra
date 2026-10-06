-- name: LockOperationIntent :one
SELECT run_id,kind,effect,instance_id,admitted_at,completed,response
			FROM knotra_operations WHERE id=$1 FOR UPDATE;

-- name: InsertOwnedOperation :execresult
INSERT INTO knotra_operations(id,run_id,kind,effect,instance_id,attempt_number,worker_id,ownership_generation,admitted_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9);

-- name: TransferPreparedOperation :execresult
UPDATE knotra_operations SET attempt_number=$2,worker_id=$3,ownership_generation=$4 WHERE id=$1;

-- name: LockOperationAdmission :one
SELECT admitted_at FROM knotra_operations WHERE id=$1 AND run_id=$2 AND instance_id=$3
			AND attempt_number=$4 AND worker_id=$5 AND ownership_generation=$6 AND kind=$7 AND NOT completed FOR UPDATE;

-- name: LockOperationScopes :many
SELECT s.id,s.limits FROM knotra_execution_scopes s JOIN knotra_execution_slots r
			ON r.run_id=s.run_id AND r.scope_id=s.id WHERE r.run_id=$1 AND r.instance_id=$2 AND r.attempt_number=$3
			AND r.ownership_generation=$4 AND r.released_at IS NULL ORDER BY s.id FOR UPDATE OF s;

-- name: SetOperationAdmitted :execresult
UPDATE knotra_operations SET admitted_at=$2 WHERE id=$1;

-- name: LockOwnedOperationResponse :one
SELECT completed,response FROM knotra_operations WHERE id=$1 AND run_id=$2 AND instance_id=$3
			AND attempt_number=$4 AND worker_id=$5 AND ownership_generation=$6 AND admitted_at IS NOT NULL FOR UPDATE;

-- name: SetOperationCompleted :execresult
UPDATE knotra_operations SET completed=true,response=$2 WHERE id=$1;
