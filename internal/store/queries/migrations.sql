-- name: CurrentSchema :one
SELECT current_schema()::text;

-- name: LockRiverMigrations :execresult
SELECT pg_advisory_lock(hashtextextended($1::text,0));
