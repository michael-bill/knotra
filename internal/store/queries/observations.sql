-- name: Observe :execresult
INSERT INTO knotra_events(run_id,sequence,document) VALUES($1,NULL,$2);
