-- Activity observations have no workflow projection sequence. PostgreSQL's
-- unique constraint continues to deduplicate every non-null projection key.
ALTER TABLE knotra_events ALTER COLUMN sequence DROP NOT NULL;

CREATE INDEX knotra_events_instance ON knotra_events(run_id,(document->>'instanceId'),id);
