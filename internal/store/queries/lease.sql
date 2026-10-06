-- name: AcquireLease :one
SELECT pg_try_advisory_lock(hashtextextended($1,0));
