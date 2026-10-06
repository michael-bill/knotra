-- Initial application schema for a new database.
CREATE TABLE knotra_artifacts (
    id text NOT NULL,
    document json NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    published boolean DEFAULT false NOT NULL
);

CREATE TABLE knotra_budgets (
    run_id text NOT NULL,
    scope text NOT NULL,
    kind text NOT NULL,
    used bigint NOT NULL
);

CREATE TABLE knotra_commands (
    principal text NOT NULL,
    id text NOT NULL,
    route text NOT NULL,
    digest text NOT NULL,
    status integer NOT NULL,
    response bytea NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL
);

CREATE TABLE knotra_definitions (
    id text NOT NULL,
    digest text NOT NULL,
    document json NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL
);

CREATE TABLE knotra_events (
    id bigint NOT NULL,
    run_id text NOT NULL,
    sequence bigint,
    document json NOT NULL
);

CREATE SEQUENCE knotra_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE knotra_events_id_seq OWNED BY knotra_events.id;

CREATE TABLE knotra_execution_attempts (
    run_id text NOT NULL,
    instance_id text NOT NULL,
    number integer NOT NULL,
    dispatch_generation bigint NOT NULL,
    dispatch_pending boolean DEFAULT true NOT NULL,
    state text NOT NULL,
    owner text,
    ownership_generation bigint DEFAULT 0 NOT NULL,
    lease_expires_at timestamptz,
    outcome_key text,
    result jsonb,
    failure jsonb,
    evidence_key text,
    outcome jsonb,
    CONSTRAINT knotra_execution_attempts_check CHECK (((state <> 'claimed'::text) OR ((owner IS NOT NULL) AND (ownership_generation > 0) AND (lease_expires_at IS NOT NULL) AND (outcome_key IS NOT NULL)))),
    CONSTRAINT knotra_execution_attempts_dispatch_generation_check CHECK ((dispatch_generation > 0)),
    CONSTRAINT knotra_execution_attempts_number_check CHECK ((number > 0)),
    CONSTRAINT knotra_execution_attempts_outcome_key_check CHECK (((outcome_key IS NULL) OR (outcome_key ~ '^[0-9a-f]{64}$'::text))),
    CONSTRAINT knotra_execution_attempts_ownership_generation_check CHECK ((ownership_generation >= 0)),
    CONSTRAINT knotra_execution_attempts_state_check CHECK ((state = ANY (ARRAY['ready'::text, 'claimed'::text, 'completed'::text, 'waiting_resolution'::text, 'cancelled'::text])))
);

CREATE TABLE knotra_execution_controls (
    run_id text NOT NULL,
    instance_id text NOT NULL,
    kind text NOT NULL,
    next_position integer DEFAULT 0 NOT NULL,
    collection_position integer DEFAULT 0 NOT NULL CHECK (collection_position >= 0 AND collection_position <= next_position),
    active_children integer DEFAULT 0 NOT NULL CHECK (active_children >= 0),
    succeeded_children integer DEFAULT 0 NOT NULL CHECK (succeeded_children >= 0),
    failed_children integer DEFAULT 0 NOT NULL CHECK (failed_children >= 0),
    iteration integer DEFAULT 0 NOT NULL,
    elements jsonb,
    element_count integer GENERATED ALWAYS AS (CASE WHEN kind='foreach' THEN jsonb_array_length(elements) ELSE 0 END) STORED,
    concurrency integer DEFAULT 0 NOT NULL CHECK (kind <> 'foreach' OR concurrency > 0),
    carry jsonb,
    child_graph_id text,
    revision bigint DEFAULT 0 NOT NULL,
    CONSTRAINT knotra_execution_controls_iteration_check CHECK ((iteration >= 0)),
    CONSTRAINT knotra_execution_controls_elements_check CHECK (kind <> 'foreach' OR (elements IS NOT NULL AND jsonb_typeof(elements)='array')),
    CONSTRAINT knotra_execution_controls_kind_check CHECK ((kind = ANY (ARRAY['foreach'::text, 'loop'::text, 'pipeline'::text]))),
    CONSTRAINT knotra_execution_controls_next_position_check CHECK ((next_position >= 0)),
    CONSTRAINT knotra_execution_controls_revision_check CHECK ((revision >= 0))
);

CREATE TABLE knotra_execution_graphs (
    run_id text NOT NULL,
    id text NOT NULL,
    parent_instance_id text,
    address text NOT NULL,
    pipeline text NOT NULL,
    graph_path text NOT NULL,
    inputs jsonb NOT NULL,
    permissions jsonb,
    scopes jsonb NOT NULL,
    deadline timestamptz NOT NULL,
    state text NOT NULL,
    materialization_cursor integer DEFAULT 0 NOT NULL,
    outputs jsonb,
    failure jsonb,
    revision bigint DEFAULT 0 NOT NULL,
    iteration_index integer,
    CONSTRAINT knotra_execution_graphs_iteration_index_check CHECK ((iteration_index >= 0)),
    CONSTRAINT knotra_execution_graphs_materialization_cursor_check CHECK ((materialization_cursor >= 0)),
    CONSTRAINT knotra_execution_graphs_revision_check CHECK ((revision >= 0)),
    CONSTRAINT knotra_execution_graphs_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'cancelled'::text])))
);

CREATE TABLE knotra_execution_nodes (
    run_id text NOT NULL,
    id text NOT NULL,
    graph_id text NOT NULL,
    node_id text NOT NULL,
    node_kind text DEFAULT '' NOT NULL,
    current_request_id text,
    dependency_node_ids text[] DEFAULT '{}' NOT NULL,
    remaining_dependencies integer DEFAULT 0 NOT NULL CHECK (remaining_dependencies BETWEEN 0 AND cardinality(dependency_node_ids)),
    address text NOT NULL,
    state text NOT NULL,
    inputs jsonb,
    outputs jsonb,
    failure jsonb,
    execution_request jsonb,
    deadline timestamptz,
    attempt_number integer DEFAULT 0 NOT NULL,
    revision bigint DEFAULT 0 NOT NULL,
    started_at timestamptz,
    finished_at timestamptz,
    reason text DEFAULT ''::text NOT NULL,
    CONSTRAINT knotra_execution_nodes_attempt_number_check CHECK ((attempt_number >= 0)),
    CONSTRAINT knotra_execution_nodes_revision_check CHECK ((revision >= 0)),
    CONSTRAINT knotra_execution_nodes_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'ready'::text, 'running'::text, 'waiting_human'::text, 'waiting_resolution'::text, 'retry_wait'::text, 'succeeded'::text, 'skipped'::text, 'failed'::text, 'cancelled'::text])))
);

CREATE TABLE knotra_execution_scopes (
    run_id text NOT NULL,
    id text NOT NULL,
    limits jsonb NOT NULL,
    materialized_instances integer DEFAULT 0 NOT NULL,
    active_attempts integer DEFAULT 0 NOT NULL,
    CONSTRAINT knotra_execution_scopes_active_attempts_check CHECK ((active_attempts >= 0)),
    CONSTRAINT knotra_execution_scopes_materialized_instances_check CHECK ((materialized_instances >= 0))
);

CREATE TABLE knotra_execution_slots (
    run_id text NOT NULL,
    instance_id text NOT NULL,
    attempt_number integer NOT NULL,
    scope_id text NOT NULL,
    ownership_generation bigint NOT NULL,
    released_at timestamptz,
    CONSTRAINT knotra_execution_slots_ownership_generation_check CHECK ((ownership_generation > 0))
);

CREATE TABLE knotra_execution_timers (
    run_id text NOT NULL,
    id text NOT NULL,
    generation bigint NOT NULL,
    instance_id text,
    kind text NOT NULL,
    due_at timestamptz NOT NULL,
    consumed_at timestamptz,
    CONSTRAINT knotra_execution_timers_generation_check CHECK ((generation > 0)),
    CONSTRAINT knotra_execution_timers_kind_check CHECK ((kind = ANY (ARRAY['run_deadline'::text, 'node_deadline'::text, 'retry'::text])))
);

CREATE TABLE knotra_instances (
    run_id text NOT NULL,
    id text NOT NULL,
    sequence bigint NOT NULL,
    document json NOT NULL
);

CREATE TABLE knotra_operations (
    id text NOT NULL,
    run_id text NOT NULL,
    kind text NOT NULL,
    effect text NOT NULL,
    completed boolean DEFAULT false NOT NULL,
    response bytea,
    created_at timestamptz DEFAULT now() NOT NULL,
    instance_id text,
    attempt_number integer,
    worker_id text,
    ownership_generation bigint,
    admitted_at timestamptz DEFAULT clock_timestamp(),
    CONSTRAINT knotra_operations_ownership CHECK ((((instance_id IS NULL) AND (attempt_number IS NULL) AND (worker_id IS NULL) AND (ownership_generation IS NULL)) OR ((instance_id IS NOT NULL) AND (attempt_number IS NOT NULL) AND (attempt_number > 0) AND (worker_id IS NOT NULL) AND (ownership_generation IS NOT NULL) AND (ownership_generation > 0))))
);

CREATE TABLE knotra_outbox (
    id bigint NOT NULL,
    run_id text NOT NULL,
    kind text NOT NULL,
    payload json NOT NULL,
    sent_at timestamptz,
    created_at timestamptz DEFAULT now() NOT NULL
);

CREATE SEQUENCE knotra_outbox_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE knotra_outbox_id_seq OWNED BY knotra_outbox.id;

CREATE TABLE knotra_requests (
    id text NOT NULL,
    run_id text NOT NULL,
    kind text NOT NULL,
    status text NOT NULL,
    document json NOT NULL,
    response_id text,
    response json,
    accepted_at timestamptz,
    created_at timestamptz DEFAULT now() NOT NULL
);

CREATE TABLE knotra_runs (
    id text NOT NULL,
    definition_id text NOT NULL,
    document json NOT NULL,
    plan json NOT NULL,
    inputs json NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    sequence bigint DEFAULT 0 NOT NULL,
    cancel_requested boolean DEFAULT false NOT NULL,
    backend text DEFAULT 'temporal'::text NOT NULL,
    scheduler_version integer DEFAULT 0 NOT NULL,
    state_format_version integer DEFAULT 0 NOT NULL,
    admitted_at timestamptz DEFAULT clock_timestamp() NOT NULL,
    execution_deadline timestamptz,
    state_revision bigint DEFAULT 0 NOT NULL,
    execution_event_sequence bigint DEFAULT 0 NOT NULL,
    wake_generation bigint DEFAULT 0 NOT NULL,
    applied_generation bigint DEFAULT 0 NOT NULL,
    dirty_since timestamptz,
    admission_paused boolean DEFAULT false NOT NULL,
    stop_cause jsonb,
    graph_cursor text DEFAULT ''::text NOT NULL,
    graph_scan_again boolean DEFAULT false NOT NULL,
    delivery_cursor text DEFAULT ''::text NOT NULL,
    CONSTRAINT knotra_runs_applied_generation_check CHECK ((applied_generation >= 0)),
    CONSTRAINT knotra_runs_backend_check CHECK ((backend = ANY (ARRAY['temporal'::text, 'river'::text]))),
    CONSTRAINT knotra_runs_execution_event_sequence_check CHECK ((execution_event_sequence >= 0)),
    CONSTRAINT knotra_runs_execution_version CHECK ((((backend = 'temporal'::text) AND (scheduler_version = 0) AND (state_format_version = 0)) OR ((backend = 'river'::text) AND (scheduler_version > 0) AND (state_format_version > 0) AND (execution_deadline IS NOT NULL)))),
    CONSTRAINT knotra_runs_generations CHECK ((applied_generation <= wake_generation)),
    CONSTRAINT knotra_runs_state_revision_check CHECK ((state_revision >= 0)),
    CONSTRAINT knotra_runs_wake_generation_check CHECK ((wake_generation >= 0))
);

CREATE TABLE knotra_settings (
    key text NOT NULL,
    value text NOT NULL
);

CREATE TABLE knotra_workers (
    id text NOT NULL,
    engine_id text NOT NULL,
    host_id text NOT NULL,
    scheduler_versions integer[] NOT NULL,
    state_format_versions integer[] NOT NULL,
    capabilities jsonb DEFAULT '{}'::jsonb NOT NULL,
    heartbeat_at timestamptz DEFAULT clock_timestamp() NOT NULL,
    draining boolean DEFAULT false NOT NULL,
    delivery_client_id text
);

ALTER TABLE ONLY knotra_events ALTER COLUMN id SET DEFAULT nextval('knotra_events_id_seq'::regclass);

ALTER TABLE ONLY knotra_outbox ALTER COLUMN id SET DEFAULT nextval('knotra_outbox_id_seq'::regclass);

ALTER TABLE ONLY knotra_artifacts
    ADD CONSTRAINT knotra_artifacts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY knotra_budgets
    ADD CONSTRAINT knotra_budgets_pkey PRIMARY KEY (run_id, scope, kind);

ALTER TABLE ONLY knotra_commands
    ADD CONSTRAINT knotra_commands_pkey PRIMARY KEY (principal, id);

ALTER TABLE ONLY knotra_definitions
    ADD CONSTRAINT knotra_definitions_digest_key UNIQUE (digest);

ALTER TABLE ONLY knotra_definitions
    ADD CONSTRAINT knotra_definitions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY knotra_events
    ADD CONSTRAINT knotra_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY knotra_events
    ADD CONSTRAINT knotra_events_run_id_sequence_key UNIQUE (run_id, sequence);

ALTER TABLE ONLY knotra_execution_attempts
    ADD CONSTRAINT knotra_execution_attempts_pkey PRIMARY KEY (run_id, instance_id, number);

ALTER TABLE ONLY knotra_execution_controls
    ADD CONSTRAINT knotra_execution_controls_pkey PRIMARY KEY (run_id, instance_id);

ALTER TABLE ONLY knotra_execution_graphs
    ADD CONSTRAINT knotra_execution_graphs_pkey PRIMARY KEY (run_id, id);

ALTER TABLE ONLY knotra_execution_graphs
    ADD CONSTRAINT knotra_execution_graphs_run_id_address_key UNIQUE (run_id, address);

ALTER TABLE ONLY knotra_execution_nodes
    ADD CONSTRAINT knotra_execution_nodes_pkey PRIMARY KEY (run_id, id);

ALTER TABLE ONLY knotra_execution_nodes
    ADD CONSTRAINT knotra_execution_nodes_run_id_address_key UNIQUE (run_id, address);

ALTER TABLE ONLY knotra_execution_nodes
    ADD CONSTRAINT knotra_execution_nodes_run_id_graph_id_node_id_key UNIQUE (run_id, graph_id, node_id);

ALTER TABLE ONLY knotra_execution_scopes
    ADD CONSTRAINT knotra_execution_scopes_pkey PRIMARY KEY (run_id, id);

ALTER TABLE ONLY knotra_execution_slots
    ADD CONSTRAINT knotra_execution_slots_pkey PRIMARY KEY (run_id, instance_id, attempt_number, scope_id);

ALTER TABLE ONLY knotra_execution_timers
    ADD CONSTRAINT knotra_execution_timers_pkey PRIMARY KEY (run_id, id, generation);

ALTER TABLE ONLY knotra_instances
    ADD CONSTRAINT knotra_instances_pkey PRIMARY KEY (run_id, id);

ALTER TABLE ONLY knotra_operations
    ADD CONSTRAINT knotra_operations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY knotra_outbox
    ADD CONSTRAINT knotra_outbox_pkey PRIMARY KEY (id);

ALTER TABLE ONLY knotra_requests
    ADD CONSTRAINT knotra_requests_pkey PRIMARY KEY (id);

ALTER TABLE ONLY knotra_runs
    ADD CONSTRAINT knotra_runs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY knotra_settings
    ADD CONSTRAINT knotra_settings_pkey PRIMARY KEY (key);

ALTER TABLE ONLY knotra_workers
    ADD CONSTRAINT knotra_workers_pkey PRIMARY KEY (id);

CREATE INDEX knotra_artifacts_published ON knotra_artifacts USING btree (id) WHERE published;

CREATE INDEX knotra_events_instance ON knotra_events USING btree (run_id, ((document ->> 'instanceId'::text)), id);

CREATE INDEX knotra_events_run ON knotra_events USING btree (run_id, id);

CREATE INDEX knotra_execution_attempts_dispatch ON knotra_execution_attempts USING btree (run_id, instance_id, number) WHERE ((state = 'ready'::text) AND dispatch_pending);

CREATE INDEX knotra_execution_attempts_expired ON knotra_execution_attempts USING btree (lease_expires_at, run_id) WHERE (state = 'claimed'::text);

CREATE INDEX knotra_execution_attempts_outcome_scan ON knotra_execution_attempts USING btree (outcome_key) INCLUDE (run_id, instance_id, number, owner, ownership_generation, lease_expires_at, state) WHERE (outcome IS NULL AND owner IS NOT NULL);

CREATE INDEX knotra_execution_attempts_ready_scan ON knotra_execution_attempts USING btree (run_id, instance_id, number) WHERE (state = 'ready'::text);

CREATE INDEX knotra_execution_graphs_active ON knotra_execution_graphs USING btree (run_id, id) WHERE (state = ANY (ARRAY['pending'::text, 'running'::text]));

CREATE INDEX knotra_execution_graphs_children ON knotra_execution_graphs USING btree (run_id, parent_instance_id, iteration_index);

CREATE UNIQUE INDEX knotra_execution_graphs_child_position ON knotra_execution_graphs (run_id,parent_instance_id,iteration_index) WHERE parent_instance_id IS NOT NULL AND iteration_index IS NOT NULL;

CREATE INDEX knotra_execution_graphs_failed_children ON knotra_execution_graphs (run_id,parent_instance_id,iteration_index) WHERE state IN ('failed','cancelled');

CREATE INDEX knotra_execution_graphs_scan ON knotra_execution_graphs USING btree (run_id, address) WHERE (state = ANY (ARRAY['pending'::text, 'running'::text]));

CREATE INDEX knotra_execution_nodes_ready ON knotra_execution_nodes USING btree (run_id, graph_id, id) WHERE (state = ANY (ARRAY['pending'::text, 'ready'::text, 'retry_wait'::text]));

CREATE INDEX knotra_execution_nodes_pending_admission ON knotra_execution_nodes (run_id,graph_id,node_id) WHERE state='pending' AND remaining_dependencies=0;

CREATE INDEX knotra_execution_nodes_active ON knotra_execution_nodes (run_id,graph_id,node_id) WHERE state NOT IN ('succeeded','skipped','failed','cancelled');

CREATE INDEX knotra_execution_nodes_dependents ON knotra_execution_nodes USING gin (dependency_node_ids) WHERE state='pending';

CREATE INDEX knotra_execution_nodes_unresolved ON knotra_execution_nodes (run_id,id) WHERE state='waiting_resolution';

CREATE INDEX knotra_execution_slots_active ON knotra_execution_slots USING btree (run_id, scope_id) WHERE (released_at IS NULL);

CREATE INDEX knotra_execution_timers_due ON knotra_execution_timers USING btree (due_at, run_id, id) WHERE (consumed_at IS NULL);

CREATE INDEX knotra_operations_unconfirmed_attempt ON knotra_operations USING btree (run_id, instance_id, attempt_number, created_at, id) WHERE ((admitted_at IS NOT NULL) AND (NOT completed));

CREATE INDEX knotra_outbox_pending ON knotra_outbox USING btree (id) WHERE (sent_at IS NULL);

CREATE INDEX knotra_requests_run ON knotra_requests USING btree (run_id);

CREATE INDEX knotra_runs_dirty ON knotra_runs USING btree (id) WHERE ((backend = 'river'::text) AND (wake_generation > applied_generation));

CREATE UNIQUE INDEX knotra_workers_delivery_client ON knotra_workers USING btree (engine_id, delivery_client_id) WHERE (delivery_client_id IS NOT NULL);

CREATE INDEX knotra_workers_heartbeat ON knotra_workers USING btree (engine_id, heartbeat_at);

ALTER TABLE ONLY knotra_budgets
    ADD CONSTRAINT knotra_budgets_run_id_fkey FOREIGN KEY (run_id) REFERENCES knotra_runs(id);

ALTER TABLE ONLY knotra_events
    ADD CONSTRAINT knotra_events_run_id_fkey FOREIGN KEY (run_id) REFERENCES knotra_runs(id);

ALTER TABLE ONLY knotra_execution_attempts
    ADD CONSTRAINT knotra_execution_attempts_owner_fkey FOREIGN KEY (owner) REFERENCES knotra_workers(id);

ALTER TABLE ONLY knotra_execution_attempts
    ADD CONSTRAINT knotra_execution_attempts_run_id_instance_id_fkey FOREIGN KEY (run_id, instance_id) REFERENCES knotra_execution_nodes(run_id, id);

ALTER TABLE ONLY knotra_execution_controls
    ADD CONSTRAINT knotra_execution_controls_run_id_child_graph_id_fkey FOREIGN KEY (run_id, child_graph_id) REFERENCES knotra_execution_graphs(run_id, id);

ALTER TABLE ONLY knotra_execution_controls
    ADD CONSTRAINT knotra_execution_controls_run_id_instance_id_fkey FOREIGN KEY (run_id, instance_id) REFERENCES knotra_execution_nodes(run_id, id);

ALTER TABLE ONLY knotra_execution_graphs
    ADD CONSTRAINT knotra_execution_graphs_parent FOREIGN KEY (run_id, parent_instance_id) REFERENCES knotra_execution_nodes(run_id, id);

ALTER TABLE ONLY knotra_execution_graphs
    ADD CONSTRAINT knotra_execution_graphs_run_id_fkey FOREIGN KEY (run_id) REFERENCES knotra_runs(id);

ALTER TABLE ONLY knotra_execution_nodes
    ADD CONSTRAINT knotra_execution_nodes_run_id_graph_id_fkey FOREIGN KEY (run_id, graph_id) REFERENCES knotra_execution_graphs(run_id, id);

ALTER TABLE ONLY knotra_execution_scopes
    ADD CONSTRAINT knotra_execution_scopes_run_id_fkey FOREIGN KEY (run_id) REFERENCES knotra_runs(id);

ALTER TABLE ONLY knotra_execution_slots
    ADD CONSTRAINT knotra_execution_slots_run_id_instance_id_attempt_number_fkey FOREIGN KEY (run_id, instance_id, attempt_number) REFERENCES knotra_execution_attempts(run_id, instance_id, number);

ALTER TABLE ONLY knotra_execution_slots
    ADD CONSTRAINT knotra_execution_slots_run_id_scope_id_fkey FOREIGN KEY (run_id, scope_id) REFERENCES knotra_execution_scopes(run_id, id);

ALTER TABLE ONLY knotra_execution_timers
    ADD CONSTRAINT knotra_execution_timers_run_id_fkey FOREIGN KEY (run_id) REFERENCES knotra_runs(id);

ALTER TABLE ONLY knotra_execution_timers
    ADD CONSTRAINT knotra_execution_timers_run_id_instance_id_fkey FOREIGN KEY (run_id, instance_id) REFERENCES knotra_execution_nodes(run_id, id);

ALTER TABLE ONLY knotra_instances
    ADD CONSTRAINT knotra_instances_run_id_fkey FOREIGN KEY (run_id) REFERENCES knotra_runs(id);

ALTER TABLE ONLY knotra_operations
    ADD CONSTRAINT knotra_operations_attempt FOREIGN KEY (run_id, instance_id, attempt_number) REFERENCES knotra_execution_attempts(run_id, instance_id, number);

ALTER TABLE ONLY knotra_operations
    ADD CONSTRAINT knotra_operations_run_id_fkey FOREIGN KEY (run_id) REFERENCES knotra_runs(id);

ALTER TABLE ONLY knotra_operations
    ADD CONSTRAINT knotra_operations_worker_id_fkey FOREIGN KEY (worker_id) REFERENCES knotra_workers(id);

ALTER TABLE ONLY knotra_outbox
    ADD CONSTRAINT knotra_outbox_run_id_fkey FOREIGN KEY (run_id) REFERENCES knotra_runs(id);

ALTER TABLE ONLY knotra_requests
    ADD CONSTRAINT knotra_requests_run_id_fkey FOREIGN KEY (run_id) REFERENCES knotra_runs(id);

ALTER TABLE ONLY knotra_runs
    ADD CONSTRAINT knotra_runs_definition_id_fkey FOREIGN KEY (definition_id) REFERENCES knotra_definitions(id);

CREATE TABLE knotra_resources (
    id text PRIMARY KEY,
    engine_id text NOT NULL,
    host_id text NOT NULL,
    run_id text NOT NULL REFERENCES knotra_runs(id),
    instance_id text NOT NULL,
    attempt_number integer NOT NULL,
    worker_id text NOT NULL REFERENCES knotra_workers(id),
    ownership_generation bigint NOT NULL CHECK (ownership_generation>0),
    kind text NOT NULL CHECK (kind IN ('sandbox','mcp_http')),
    mcp_session jsonb CHECK (mcp_session IS NULL OR (kind='mcp_http' AND jsonb_typeof(mcp_session)='object')),
    cleanup_generation bigint NOT NULL DEFAULT 1 CHECK (cleanup_generation>0),
    lifetime text NOT NULL CHECK (lifetime IN ('attempt','run')),
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','cleaning','closed')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    closed_at timestamptz,
    FOREIGN KEY(run_id,instance_id,attempt_number)
        REFERENCES knotra_execution_attempts(run_id,instance_id,number)
);
CREATE INDEX knotra_resources_cleanup ON knotra_resources(engine_id,host_id,id)
    WHERE state<>'closed';
