-- name: ReadSchedulerMetrics :one
-- One snapshot and one database clock. Future timers and terminal runs
-- do not contribute to backlog ages. River delivery is read through its API.
SELECT
    statement_timestamp()::timestamptz AS observed_at,
    COALESCE((SELECT max(GREATEST(0,extract(epoch FROM (statement_timestamp()-dirty_since))))
        FROM knotra_runs WHERE backend='river' AND wake_generation>applied_generation
        AND document->>'status' NOT IN ('succeeded','failed','cancelled','skipped')),0)::double precision AS dirty_age,
    (SELECT count(*) FROM knotra_execution_attempts
        WHERE state='claimed' AND lease_expires_at<=statement_timestamp())::bigint AS expired_leases,
    (SELECT count(*) FROM knotra_execution_nodes WHERE state='waiting_resolution')::bigint AS unknown_outcomes,
    (SELECT count(*) FROM knotra_execution_slots WHERE released_at IS NULL)::bigint AS slot_reservations,
    COALESCE((SELECT max(GREATEST(0,extract(epoch FROM (statement_timestamp()-t.due_at))))
        FROM knotra_execution_timers t JOIN knotra_runs r ON r.id=t.run_id
        WHERE t.consumed_at IS NULL AND t.due_at<=statement_timestamp()
        AND r.document->>'status' NOT IN ('succeeded','failed','cancelled','skipped')),0)::double precision AS timer_lag,
    ARRAY(SELECT outcome_key FROM knotra_execution_attempts
        WHERE outcome IS NULL AND owner IS NOT NULL AND outcome_key IS NOT NULL)::text[] AS pending_keys;
