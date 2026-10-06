-- name: LockRunProjection :one
SELECT document,sequence FROM knotra_runs WHERE id=$1 FOR UPDATE;

-- name: EventSequenceExists :one
SELECT EXISTS(SELECT 1 FROM knotra_events WHERE run_id=$1 AND sequence=$2);

-- name: PublishArtifact :execresult
UPDATE knotra_artifacts SET published=true WHERE id=sqlc.arg(id) AND document->'origin'->>'runId'=sqlc.arg(origin_run_id)::text;

-- name: UpsertInstanceProjection :execresult
INSERT INTO knotra_instances(run_id,id,sequence,document) VALUES($1,$2,$3,$4) ON CONFLICT(run_id,id) DO UPDATE SET sequence=EXCLUDED.sequence,document=EXCLUDED.document WHERE knotra_instances.sequence<EXCLUDED.sequence;

-- name: CancelOpenRequests :execresult
UPDATE knotra_requests SET status='cancelled' WHERE run_id=$1 AND status IN ('open','pending');

-- name: UpdateRunProjection :execresult
UPDATE knotra_runs SET document=$2,sequence=$3 WHERE id=$1;

-- name: InsertExecutionEvent :execresult
INSERT INTO knotra_events(run_id,sequence,document) VALUES($1,$2,$3);

-- name: UpsertRequest :execresult
INSERT INTO knotra_requests(id,run_id,kind,status,document) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT(id) DO UPDATE SET status=EXCLUDED.status,document=EXCLUDED.document
 WHERE knotra_requests.status IN ('open','pending') AND EXCLUDED.status NOT IN ('open','pending');

-- name: ReadRequest :one
SELECT document,status FROM knotra_requests WHERE id=$1;

-- name: ListHumanRequests :many
-- Bound raw documents before sqlc materializes them; keep one cursor marker.
WITH candidates AS (
    SELECT id,octet_length(document::text) AS bytes
    FROM knotra_requests
    WHERE kind='human' AND (sqlc.arg(cursor)::text='' OR id<sqlc.arg(cursor))
    ORDER BY id DESC
    LIMIT 101
), page AS (
    SELECT id,row_number() OVER (ORDER BY id DESC) AS position,
        COALESCE(sum(bytes) OVER (ORDER BY id DESC ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING),0) AS preceding_bytes
    FROM candidates
)
SELECT d.document,d.status,d.created_at
FROM knotra_requests d JOIN page p ON p.id=d.id
WHERE p.position<=2 OR p.preceding_bytes<=sqlc.arg(max_bytes)::bigint
ORDER BY d.id DESC;

-- name: LockTransactionKey :execresult
SELECT pg_advisory_xact_lock(hashtextextended($1,0));

-- name: ReadBudgetUsed :one
SELECT used FROM knotra_budgets WHERE run_id=$1 AND scope=$2 AND kind=$3;

-- name: IncrementBudget :execresult
INSERT INTO knotra_budgets(run_id,scope,kind,used) VALUES($1,$2,$3,1) ON CONFLICT(run_id,scope,kind) DO UPDATE SET used=knotra_budgets.used+1;

-- name: InsertLegacyOperation :execresult
INSERT INTO knotra_operations(id,run_id,kind,effect)
		 SELECT sqlc.arg(id),r.id,sqlc.arg(kind),sqlc.arg(effect) FROM knotra_runs r WHERE r.id=sqlc.arg(run_id) AND backend='temporal' ON CONFLICT DO NOTHING;

-- name: ReadLegacyOperation :one
SELECT o.completed,o.response FROM knotra_operations o JOIN knotra_runs r ON r.id=o.run_id
		WHERE o.id=$1 AND o.run_id=$2 AND r.backend='temporal';

-- name: CompleteLegacyOperation :execresult
UPDATE knotra_operations SET completed=true,response=$2 WHERE id=$1 AND NOT completed AND worker_id IS NULL;

-- name: ReadLegacyOperationResponse :one
SELECT response FROM knotra_operations WHERE id=$1 AND completed AND worker_id IS NULL;

-- name: LockHumanAnswer :one
SELECT status,response_id,response,accepted_at FROM knotra_requests WHERE id=$1 AND run_id=$2 FOR UPDATE;

-- name: CloseRequest :execresult
UPDATE knotra_requests SET status=$2 WHERE id=$1 AND status IN ('open','pending');
