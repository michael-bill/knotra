-- name: ReadExecutionGraph :one
SELECT to_jsonb(g) FROM knotra_execution_graphs g WHERE run_id=$1 AND id=$2;

-- name: SaveExecutionRequest :execresult
UPDATE knotra_requests SET status=$3,document=$4
		WHERE run_id=$1 AND id=$2 AND response_id=$5 AND
		((kind='human' AND status='answered' AND $3='accepted') OR (kind='resolution' AND status='resolved' AND $3='resolved'));

-- name: InsertExecutionGraph :execresult
INSERT INTO knotra_execution_graphs(run_id,id,parent_instance_id,address,pipeline,graph_path,
		inputs,permissions,scopes,deadline,state,revision,iteration_index) VALUES(sqlc.arg(run_id),sqlc.arg(id),NULLIF(sqlc.arg(parent_instance_id)::text,''),sqlc.arg(address),sqlc.arg(pipeline),sqlc.arg(graph_path),sqlc.arg(inputs),sqlc.arg(permissions),sqlc.arg(scopes),sqlc.arg(deadline),sqlc.arg(state),sqlc.arg(revision),sqlc.arg(iteration_index));

-- name: SaveExecutionGraph :execresult
UPDATE knotra_execution_graphs SET state=sqlc.arg(state),materialization_cursor=sqlc.arg(materialization_cursor),
		inputs=CASE WHEN state='pending' THEN sqlc.arg(inputs)::jsonb ELSE inputs END,outputs=sqlc.arg(outputs),failure=sqlc.arg(failure),revision=sqlc.arg(revision)
		WHERE run_id=sqlc.arg(run_id) AND id=sqlc.arg(id) AND revision=sqlc.arg(revision)-1 AND state IN ('pending','running');

-- name: SaveExecutionNode :execresult
INSERT INTO knotra_execution_nodes(run_id,id,graph_id,node_id,address,state,inputs,outputs,failure,
		execution_request,deadline,attempt_number,revision,started_at,finished_at,reason,node_kind,dependency_node_ids,remaining_dependencies,current_request_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,NULLIF(sqlc.arg(current_request_id)::text,''))
		ON CONFLICT(run_id,id) DO UPDATE SET state=EXCLUDED.state,inputs=EXCLUDED.inputs,outputs=EXCLUDED.outputs,
		failure=EXCLUDED.failure,execution_request=EXCLUDED.execution_request,deadline=EXCLUDED.deadline,current_request_id=EXCLUDED.current_request_id,
		attempt_number=EXCLUDED.attempt_number,revision=EXCLUDED.revision,started_at=EXCLUDED.started_at,
		finished_at=EXCLUDED.finished_at,reason=EXCLUDED.reason
		WHERE knotra_execution_nodes.revision=EXCLUDED.revision-1
		AND knotra_execution_nodes.state NOT IN ('succeeded','skipped','failed','cancelled')
		AND (knotra_execution_nodes.deadline IS NULL OR knotra_execution_nodes.deadline=EXCLUDED.deadline);

-- name: InsertExecutionAttempt :execresult
INSERT INTO knotra_execution_attempts(run_id,instance_id,number,dispatch_generation,dispatch_pending,state)
		VALUES($1,$2,$3,$4,$5,'ready');

-- name: ReadExecutionAttempts :many
SELECT (to_jsonb(a) || jsonb_build_object(
    'runId',a.run_id,'instanceId',a.instance_id,'Ownership',CASE WHEN a.owner IS NULL THEN NULL ELSE jsonb_build_object(
    'runId',a.run_id,'instanceId',a.instance_id,'number',a.number,'workerId',a.owner,'generation',a.ownership_generation) END))::jsonb
FROM knotra_execution_attempts a JOIN knotra_execution_nodes n ON n.run_id=a.run_id AND n.id=a.instance_id
WHERE a.run_id=$1 AND n.graph_id=$2 AND a.number=n.attempt_number
    AND (sqlc.narg(instance_ids)::text[] IS NULL OR n.id=ANY(sqlc.narg(instance_ids)::text[])) ORDER BY a.instance_id;

-- name: ReadExecutionScopes :many
SELECT to_jsonb(s) FROM knotra_execution_scopes s WHERE run_id=sqlc.arg(run_id) AND id=ANY(sqlc.arg(scope_i_ds)::text[]) ORDER BY id FOR UPDATE;

-- name: ReadExecutionRetryTimers :many
SELECT to_jsonb(t) FROM knotra_execution_timers t
JOIN knotra_execution_nodes n ON n.run_id=t.run_id AND n.id=t.instance_id
WHERE t.run_id=$1 AND n.graph_id=$2 AND t.kind='retry' AND t.generation=n.attempt_number
    AND (sqlc.narg(instance_ids)::text[] IS NULL OR n.id=ANY(sqlc.narg(instance_ids)::text[]));

-- name: ReadExecutionRequests :many
SELECT (q.document::jsonb || jsonb_build_object(
    'status',q.status,'responseId',q.response_id,'acceptedAt',q.accepted_at,'answer',q.response))::jsonb
FROM knotra_requests q JOIN knotra_execution_nodes n ON n.run_id=q.run_id AND n.current_request_id=q.id
WHERE q.run_id=$1 AND n.graph_id=$2 AND n.state IN ('waiting_human','waiting_resolution')
    AND (sqlc.narg(instance_ids)::text[] IS NULL OR n.id=ANY(sqlc.narg(instance_ids)::text[])) ORDER BY q.id;

-- name: ReadExecutionControls :many
SELECT jsonb_build_object('run_id',c.run_id,'instance_id',c.instance_id,'kind',c.kind,
    'next_position',c.next_position,'collection_position',c.collection_position,'active_children',c.active_children,
    'succeeded_children',c.succeeded_children,'failed_children',c.failed_children,'iteration',c.iteration,
    'element_count',c.element_count,'concurrency',c.concurrency,'current_element',c.elements->c.next_position,
    'carry',c.carry,'child_graph_id',c.child_graph_id,'revision',c.revision)::jsonb
FROM knotra_execution_controls c
JOIN knotra_execution_nodes n ON n.run_id=c.run_id AND n.id=c.instance_id
WHERE c.run_id=$1 AND n.graph_id=$2
    AND (sqlc.narg(instance_ids)::text[] IS NULL OR n.id=ANY(sqlc.narg(instance_ids)::text[]))
ORDER BY c.instance_id;

-- name: ReadChildExecutionGraphs :many
SELECT CASE WHEN sqlc.narg(instance_ids)::text[] IS NULL THEN to_jsonb(children) ELSE
    jsonb_build_object('run_id',children.run_id,'id',children.id,'parent_instance_id',children.parent_instance_id,
        'state',children.state,'outputs',children.outputs,'failure',children.failure,'iteration_index',children.iteration_index)
    END::jsonb
FROM (
    SELECT g.*
    FROM knotra_execution_nodes n
    JOIN knotra_execution_controls c ON c.run_id=n.run_id AND c.instance_id=n.id
    JOIN LATERAL (
        (SELECT g.* FROM knotra_execution_graphs g
        WHERE g.run_id=c.run_id AND g.parent_instance_id=c.instance_id AND g.state IN ('failed','cancelled')
        ORDER BY g.iteration_index,g.id LIMIT 1)
        UNION ALL
        (SELECT g.* FROM knotra_execution_graphs g
        WHERE g.run_id=c.run_id AND g.parent_instance_id=c.instance_id AND g.state='succeeded'
            AND c.active_children=0 AND c.failed_children=0
            AND c.next_position=c.element_count
            AND g.iteration_index>=c.collection_position
        ORDER BY g.iteration_index,g.id LIMIT 64)
    ) g ON true
    WHERE n.run_id=$1 AND n.graph_id=$2 AND c.kind='foreach'
        AND (sqlc.narg(instance_ids)::text[] IS NULL OR n.id=ANY(sqlc.narg(instance_ids)::text[]))
    UNION ALL
    SELECT g.*
    FROM knotra_execution_nodes n
    JOIN knotra_execution_controls c ON c.run_id=n.run_id AND c.instance_id=n.id
    JOIN knotra_execution_graphs g ON g.run_id=c.run_id AND g.id=c.child_graph_id
    WHERE n.run_id=$1 AND n.graph_id=$2 AND c.kind IN ('pipeline','loop')
        AND (sqlc.narg(instance_ids)::text[] IS NULL OR n.id=ANY(sqlc.narg(instance_ids)::text[]))
) children
ORDER BY children.parent_instance_id,children.iteration_index,children.id;

-- name: SaveExecutionControl :execresult
INSERT INTO knotra_execution_controls (
    run_id,instance_id,kind,next_position,collection_position,iteration,elements,carry,child_graph_id,revision,concurrency
) VALUES (sqlc.arg(run_id),sqlc.arg(instance_id),sqlc.arg(kind),sqlc.arg(next_position),sqlc.arg(collection_position),
    sqlc.arg(iteration),COALESCE(sqlc.arg(elements)::jsonb,'[]'::jsonb),sqlc.arg(carry),NULLIF(sqlc.arg(child_graph_id)::text,''),sqlc.arg(revision),sqlc.arg(concurrency))
ON CONFLICT (run_id,instance_id) DO UPDATE SET
    next_position=EXCLUDED.next_position,
    collection_position=EXCLUDED.collection_position,
    iteration=EXCLUDED.iteration,
    carry=EXCLUDED.carry,
    child_graph_id=EXCLUDED.child_graph_id,
    revision=EXCLUDED.revision
WHERE knotra_execution_controls.kind=EXCLUDED.kind
    AND knotra_execution_controls.revision=EXCLUDED.revision-1;

-- name: AddExecutionChild :execresult
UPDATE knotra_execution_controls SET active_children=active_children+1
WHERE run_id=$1 AND instance_id=$2;

-- name: FinishExecutionChild :execresult
UPDATE knotra_execution_controls SET active_children=active_children-1,
    succeeded_children=succeeded_children+CASE WHEN sqlc.arg(state)::text='succeeded' THEN 1 ELSE 0 END,
    failed_children=failed_children+CASE WHEN sqlc.arg(state)::text IN ('failed','cancelled') THEN 1 ELSE 0 END
WHERE run_id=sqlc.arg(run_id) AND instance_id=sqlc.arg(instance_id) AND active_children>0;

-- name: ReadForeachCollectedOutputs :one
SELECT COALESCE(jsonb_object_agg(port.name,port.value),'{}'::jsonb)::jsonb
FROM (
    SELECT p.name,CASE WHEN p.artifact THEN jsonb_build_object('collection',true,'artifacts',
        COALESCE(jsonb_agg(g.outputs->p.name->'artifacts'->0 ORDER BY g.iteration_index) FILTER (WHERE g.id IS NOT NULL),'[]'::jsonb))
    ELSE jsonb_build_object('json',COALESCE(jsonb_agg(g.outputs->p.name->'json' ORDER BY g.iteration_index) FILTER (WHERE g.id IS NOT NULL),'[]'::jsonb)) END AS value
    FROM (
        SELECT unnest(sqlc.arg(json_ports)::text[]) AS name,false AS artifact
        UNION ALL
        SELECT unnest(sqlc.arg(artifact_ports)::text[]) AS name,true AS artifact
    ) p
    LEFT JOIN knotra_execution_graphs g ON g.run_id=sqlc.arg(run_id) AND g.parent_instance_id=sqlc.arg(instance_id) AND g.state='succeeded'
    GROUP BY p.name,p.artifact
) port;

-- name: InsertExecutionScope :execresult
INSERT INTO knotra_execution_scopes(run_id,id,limits) VALUES($1,$2,$3);

-- name: ListRunnableGraphs :many
SELECT g.id,g.address
FROM knotra_execution_graphs g
WHERE g.run_id=$1 AND g.state IN ('pending','running')
AND (sqlc.arg(focus_instance_id)::text='' OR EXISTS(
    SELECT 1 FROM knotra_execution_nodes n WHERE n.run_id=g.run_id
        AND n.graph_id=g.id AND n.id=sqlc.arg(focus_instance_id)::text))
AND (
    g.address>sqlc.arg(graph_cursor)::text
    OR EXISTS(SELECT 1 FROM knotra_execution_nodes n WHERE n.run_id=g.run_id
        AND n.graph_id=g.id AND n.id=sqlc.arg(focus_instance_id)::text)
    OR EXISTS(SELECT 1 FROM knotra_execution_nodes n JOIN knotra_requests q
        ON q.run_id=n.run_id AND q.id=n.current_request_id
        WHERE n.run_id=g.run_id AND n.graph_id=g.id
            AND n.state IN ('waiting_human','waiting_resolution')
            AND q.status IN ('answered','resolved'))
)
ORDER BY
    EXISTS(SELECT 1 FROM knotra_execution_nodes n WHERE n.run_id=g.run_id
        AND n.graph_id=g.id AND n.id=sqlc.arg(focus_instance_id)::text) DESC,
    EXISTS(SELECT 1 FROM knotra_execution_nodes n JOIN knotra_requests q
        ON q.run_id=n.run_id AND q.id=n.current_request_id
        WHERE n.run_id=g.run_id AND n.graph_id=g.id
            AND n.state IN ('waiting_human','waiting_resolution')
            AND q.status IN ('answered','resolved')) DESC,
    g.address
LIMIT 64;

-- name: SetGraphCursor :execresult
UPDATE knotra_runs SET graph_cursor=$2,state_revision=state_revision+1 WHERE id=$1;

-- name: HasActiveExecution :one
SELECT EXISTS(
    SELECT 1 FROM knotra_execution_graphs g WHERE g.run_id=$1 AND g.state IN ('pending','running')
    UNION ALL
    SELECT 1 FROM knotra_execution_attempts a WHERE a.run_id=$1 AND a.state='claimed'
);

-- name: MarkGraphScanProgress :execresult
UPDATE knotra_runs SET graph_scan_again=true WHERE id=$1;

-- name: FinishGraphScan :execresult
UPDATE knotra_runs SET graph_cursor='',graph_scan_again=false,state_revision=state_revision+1 WHERE id=$1;

-- name: ReleaseNodeDependencies :execresult
UPDATE knotra_execution_nodes SET remaining_dependencies=remaining_dependencies-1
WHERE run_id=$1 AND graph_id=$2 AND state='pending'
    AND dependency_node_ids @> ARRAY[sqlc.arg(node_id)::text];

-- name: ReadSchedulableNodes :many
WITH selected AS MATERIALIZED (
    SELECT n.id,n.node_id,COALESCE(q.status IN ('answered','resolved'),false) AS answered
    FROM knotra_execution_nodes n
    LEFT JOIN knotra_execution_attempts a ON a.run_id=n.run_id AND a.instance_id=n.id AND a.number=n.attempt_number
    LEFT JOIN knotra_execution_controls c ON c.run_id=n.run_id AND c.instance_id=n.id
    LEFT JOIN knotra_execution_graphs child ON child.run_id=c.run_id AND child.id=c.child_graph_id
    LEFT JOIN knotra_requests q ON q.run_id=n.run_id AND q.id=n.current_request_id
    WHERE n.run_id=sqlc.arg(run_id) AND n.graph_id=sqlc.arg(graph_id)
        AND n.state NOT IN ('succeeded','skipped','failed','cancelled') AND (
        sqlc.arg(stopping)::boolean OR n.deadline<=sqlc.arg(now)::timestamptz
        OR n.id=sqlc.arg(focus_instance_id)::text
        OR (n.state='pending' AND n.remaining_dependencies=0 AND NOT sqlc.arg(paused)::boolean)
        OR (n.state IN ('ready','running') AND a.state='completed')
        OR (n.state='ready' AND a.dispatch_pending AND NOT sqlc.arg(paused)::boolean)
        OR (n.state='retry_wait' AND NOT sqlc.arg(paused)::boolean AND EXISTS(
            SELECT 1 FROM knotra_execution_timers t WHERE t.run_id=n.run_id AND t.instance_id=n.id
                AND t.kind='retry' AND t.generation=n.attempt_number AND t.due_at<=sqlc.arg(now)::timestamptz))
        OR (n.state IN ('waiting_human','waiting_resolution') AND q.status IN ('answered','resolved'))
        OR (n.state='running' AND (
            n.node_kind='switch'
            OR (n.node_kind IN ('pipeline','loop','foreach') AND c.instance_id IS NULL)
            OR (c.kind IN ('pipeline','loop') AND (child.state IN ('succeeded','failed','cancelled')
                OR (c.child_graph_id IS NULL AND NOT sqlc.arg(paused)::boolean)))
            OR (c.kind='foreach' AND (c.failed_children>0
                OR (c.active_children=0 AND c.next_position=c.element_count)
                OR (NOT sqlc.arg(paused)::boolean AND c.next_position<c.element_count
                    AND c.active_children<c.concurrency)))
        ))
    )
    ORDER BY n.id=sqlc.arg(focus_instance_id)::text DESC,answered DESC,n.node_id
    LIMIT LEAST(64,sqlc.arg(node_limit)::integer)
)
SELECT CASE WHEN n.node_kind='foreach' AND c.instance_id IS NOT NULL THEN
    jsonb_build_object('run_id',n.run_id,'id',n.id,'graph_id',n.graph_id,'node_id',n.node_id,'address',n.address,
        'state',n.state,'outputs',n.outputs,'failure',n.failure,'deadline',n.deadline,'attempt_number',n.attempt_number,
        'revision',n.revision,'started_at',n.started_at,'finished_at',n.finished_at,'reason',n.reason,'inputs_omitted',true)
    ELSE to_jsonb(n) END::jsonb
FROM selected s JOIN knotra_execution_nodes n ON n.run_id=sqlc.arg(run_id) AND n.id=s.id
LEFT JOIN knotra_execution_controls c ON c.run_id=n.run_id AND c.instance_id=n.id
ORDER BY n.id=sqlc.arg(focus_instance_id)::text DESC,s.answered DESC,s.node_id;

-- name: ReadExecutionDependencyValues :many
SELECT jsonb_build_object('run_id',n.run_id,'id',n.id,'graph_id',n.graph_id,
    'node_id',n.node_id,'state',n.state,'outputs',n.outputs)::jsonb
FROM knotra_execution_nodes n WHERE n.run_id=$1 AND n.graph_id=$2
    AND n.node_id=ANY(sqlc.arg(node_ids)::text[]) ORDER BY n.node_id;

-- name: HasActiveNodesOutsideSelection :one
SELECT EXISTS(SELECT 1 FROM knotra_execution_nodes WHERE run_id=$1 AND graph_id=$2
    AND state NOT IN ('succeeded','skipped','failed','cancelled') AND NOT (id=ANY(sqlc.arg(instance_ids)::text[])));

-- name: HasUnresolvedOutsideSelection :one
SELECT EXISTS(SELECT 1 FROM knotra_execution_nodes WHERE run_id=$1
    AND state='waiting_resolution' AND NOT (id=ANY(sqlc.arg(instance_ids)::text[])));

-- name: ReadExecutionGraphMetadata :one
SELECT jsonb_build_object('run_id',g.run_id,'id',g.id,'parent_instance_id',g.parent_instance_id,
    'address',g.address,'pipeline',g.pipeline,'graph_path',g.graph_path,'permissions',g.permissions,
    'scopes',g.scopes,'deadline',g.deadline,'state',g.state,'materialization_cursor',g.materialization_cursor,
    'outputs',g.outputs,'failure',g.failure,'revision',g.revision,'iteration_index',g.iteration_index)::jsonb
FROM knotra_execution_graphs g WHERE g.run_id=$1 AND g.id=$2;

-- name: ReadExecutionGraphInputValues :one
SELECT CASE WHEN sqlc.narg(port_names)::text[] IS NULL THEN g.inputs ELSE
    (SELECT COALESCE(jsonb_object_agg(k.name,g.inputs->k.name) FILTER (WHERE g.inputs ? k.name),'{}'::jsonb)
        FROM unnest(sqlc.narg(port_names)::text[]) k(name)) END::jsonb
FROM knotra_execution_graphs g WHERE g.run_id=$1 AND g.id=$2;

-- name: ReadExecutionNodeInputValues :one
SELECT (SELECT COALESCE(jsonb_object_agg(k.name,n.inputs->k.name) FILTER (WHERE n.inputs ? k.name),'{}'::jsonb)
    FROM unnest(sqlc.arg(port_names)::text[]) k(name))::jsonb
FROM knotra_execution_nodes n WHERE n.run_id=$1 AND n.id=$2;

-- name: ReadExecutionNode :one
SELECT to_jsonb(n) FROM knotra_execution_nodes n WHERE n.run_id=$1 AND n.id=$2;
