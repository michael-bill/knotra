-- Uploaded artifacts are immediately public. Runtime artifacts remain staged
-- until their producing node publishes validated outputs in the same transaction.
ALTER TABLE knotra_artifacts ADD COLUMN published boolean NOT NULL DEFAULT false;
UPDATE knotra_artifacts SET published=true
WHERE document->'origin'->>'runId' IS NULL
   OR EXISTS (
       SELECT 1 FROM knotra_instances i
       WHERE i.run_id=knotra_artifacts.document->'origin'->>'runId'
         AND i.id=knotra_artifacts.document->'origin'->>'instanceId'
         AND i.document->>'status'='succeeded'
   );
CREATE INDEX knotra_artifacts_published ON knotra_artifacts(id) WHERE published;
