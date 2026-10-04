import { useEffect, useState } from 'react';
import { ArrowLeft, Play, Search, XCircle } from 'lucide-react';
import { engineError, storedEvents, watchRun } from '../lib/engine/client';
import type { EngineEvent, EngineRun } from '../lib/engine/types';
import type { EngineController } from '../lib/engine/useEngine';
import { Empty, Modal, Status, time } from './ui';
import { Diagnostics } from './EngineDiagnostics';
import SourceEditor from './SourceEditor';

export function EngineRunsView({
  engine,
  activeId,
  onSelect,
  onReview,
}: {
  engine: EngineController;
  activeId?: string;
  onSelect: (id?: string) => void;
  onReview: () => void;
}) {
  const run = engine.runs.find((run) => run.id === activeId);
  const [search, setSearch] = useState('');
  const [status, setStatus] = useState('all');
  if (run)
    return (
      <EngineRunDetail
        key={run.id}
        run={run}
        engine={engine}
        onBack={() => onSelect(undefined)}
        onReview={onReview}
      />
    );
  const filtered = engine.runs.filter(
    (run) =>
      (status === 'all' || status === run.status) &&
      `${run.id} ${run.title}`.toLowerCase().includes(search.toLowerCase()),
  );
  return (
    <section className="page">
      <header className="page-heading">
        <div>
          <h1>Engine runs</h1>
          <p>Execution state belongs to the engine. Closing this app keeps runs alive.</p>
        </div>
      </header>
      <div className="list-toolbar runs-toolbar">
        <label className="search-field">
          <Search size={16} />
          <input
            aria-label="Search engine runs"
            placeholder="Search runs…"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
        </label>
        <label className="status-filter">
          <select
            aria-label="Filter engine runs"
            value={status}
            onChange={(event) => setStatus(event.target.value)}
          >
            {[
              'all',
              'pending',
              'ready',
              'running',
              'retry_wait',
              'waiting_human',
              'waiting_resolution',
              'succeeded',
              'failed',
              'cancelled',
            ].map((status) => (
              <option value={status} key={status}>
                {status === 'all' ? 'All statuses' : status.replaceAll('_', ' ')}
              </option>
            ))}
          </select>
        </label>
      </div>
      {filtered.length ? (
        <div className="run-table">
          <div className="table-header">
            <span>PIPELINE / RUN</span>
            <span>STATUS</span>
            <span>STARTED</span>
            <span>PROFILE</span>
          </div>
          {filtered.map((run) => (
            <button className="table-row" key={run.id} onClick={() => onSelect(run.id)}>
              <span className="run-name">
                <Play size={17} />
                <span>
                  <strong>{run.title}</strong>
                  <small>{run.id}</small>
                </span>
              </span>
              <Status status={run.status} />
              <span className="small muted">{time(run.createdAt)}</span>
              <span className="small">{run.profile}</span>
            </button>
          ))}
        </div>
      ) : (
        <Empty icon={<Play />} title="No engine runs">
          Start a workflow from its editor after connecting an engine.
        </Empty>
      )}
    </section>
  );
}

function EngineRunDetail({
  run,
  engine,
  onBack,
  onReview,
}: {
  run: EngineRun;
  engine: EngineController;
  onBack: () => void;
  onReview: () => void;
}) {
  const [tab, setTab] = useState('instances');
  const [events, setEvents] = useState<EngineEvent[]>([]);
  const [stream, setStream] = useState('Connecting…');
  const [resolution, setResolution] = useState('');
  const [evidence, setEvidence] = useState('');
  const [outcome, setOutcome] = useState<'succeeded' | 'not_started' | 'failed'>('failed');
  const [outputs, setOutputs] = useState('{}');
  const [resolutionError, setResolutionError] = useState('');
  const [busy, setBusy] = useState(false);
  const refresh = engine.refresh;
  useEffect(() => {
    let stopped = false;
    let stop: (() => void) | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    setEvents([]);
    if (!engine.info) {
      setStream('Disconnected · cached run');
      return;
    }
    void storedEvents(run.id)
      .then((events) => {
        if (!stopped)
          setEvents((current) =>
            [
              ...events,
              ...current.filter((event) => !events.some((saved) => saved.id === event.id)),
            ].slice(-1000),
          );
      })
      .catch(() => {});
    // Completed runs still need their authoritative history when opened on a new client.
    void watchRun(run.id, (message) => {
      if (stopped) return;
      if (message.type === 'connection')
        setStream(message.message ? `${message.status} · ${message.message}` : message.status);
      else {
        setEvents((events) =>
          events.some((event) => event.id === message.event.id)
            ? events
            : [...events, message.event].slice(-1000),
        );
        if (!timer)
          timer = setTimeout(() => {
            timer = undefined;
            void refresh();
          }, 200);
      }
    })
      .then((cancel) => {
        if (stopped) cancel();
        else stop = cancel;
      })
      .catch((error) => {
        if (!stopped) setStream(engineError(error).message);
      });
    return () => {
      stopped = true;
      stop?.();
      if (timer) clearTimeout(timer);
    };
  }, [run.id, engine.info, refresh]);
  async function act(op: 'cancel' | 'resume') {
    setBusy(true);
    try {
      await engine.command({ op, runId: run.id, operationId: crypto.randomUUID() });
    } catch {
      /* Toast contains the engine response. */
    } finally {
      setBusy(false);
    }
  }
  async function resolve() {
    setBusy(true);
    setResolutionError('');
    try {
      const values = outcome === 'succeeded' ? JSON.parse(outputs) : {};
      if (!values || typeof values !== 'object' || Array.isArray(values))
        throw new Error('Resolution outputs must be an object.');
      await engine.command({
        op: 'resolve',
        runId: run.id,
        instanceId: resolution,
        resolution: { outcome, evidence, ...(outcome === 'succeeded' ? { outputs: values } : {}) },
        operationId: crypto.randomUUID(),
      });
      setResolution('');
    } catch (error) {
      setResolutionError(engineError(error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="run-detail">
      <div className="run-heading">
        <button className="text-button" onClick={onBack}>
          <ArrowLeft size={15} />
          All engine runs
        </button>
        <span className="type-label">ENGINE RUN</span>
      </div>
      <header className="page-heading">
        <div>
          <h1>{run.title}</h1>
          <p>
            {run.id} · {run.profile} · {time(run.createdAt)}
          </p>
        </div>
        <div className="heading-actions">
          <Status status={run.status} />
          {engine.requests.some(
            (request) => request.runId === run.id && request.status === 'open',
          ) ? (
            <button className="button primary" onClick={onReview}>
              Review request
            </button>
          ) : null}
          {run.availableActions
            .filter((action) => action !== 'resolve')
            .map((action) => (
              <button
                key={action}
                className="button"
                disabled={
                  !engine.info ||
                  busy ||
                  engine.pending.some(
                    (item) => 'runId' in item.request && item.request.runId === run.id,
                  )
                }
                onClick={() => void act(action as 'cancel' | 'resume')}
              >
                {action === 'cancel' ? <XCircle size={15} /> : <Play size={15} />}
                {action === 'cancel' ? 'Request cancellation' : 'Resume saved attempt'}
              </button>
            ))}
        </div>
      </header>
      <Diagnostics diagnostics={run.diagnostics} />
      <div className="underline-tabs">
        {['instances', 'timeline', 'inputs', 'outputs', 'snapshot'].map((tabName) => (
          <button
            key={tabName}
            className={tab === tabName ? 'active' : ''}
            onClick={() => setTab(tabName)}
          >
            {tabName[0].toUpperCase() + tabName.slice(1)}
          </button>
        ))}
      </div>
      <div className="engine-run-content">
        {tab === 'snapshot' ? (
          <SourceEditor source={run.package.source} readOnly label="Engine immutable snapshot" />
        ) : tab === 'timeline' ? (
          <>
            <p className="small muted">{stream}</p>
            <div className="timeline">
              {events.map((event) => (
                <div className="timeline-event" key={event.id}>
                  <span className="timeline-marker" />
                  <time>{time(event.at)}</time>
                  <div>
                    <strong>{event.instanceId ?? event.type}</strong>
                    <p>{event.message}</p>
                    <small>
                      {event.attemptId} {event.operationId}
                    </small>
                  </div>
                </div>
              ))}
            </div>
          </>
        ) : tab === 'instances' ? (
          <div className="engine-instance-list">
            {run.instances.map((instance) => (
              <article key={instance.id}>
                <div className="inline spread">
                  <div>
                    <strong>{instance.nodeId}</strong>
                    <code>{instance.id}</code>
                    <small>
                      {instance.scope || 'Root graph'} · {instance.attemptId ?? 'No attempt'}
                    </small>
                  </div>
                  <Status status={instance.status} />
                </div>
                {instance.error ? <Diagnostics diagnostics={[instance.error]} /> : null}
                {instance.status === 'waiting_resolution' &&
                run.availableActions.includes('resolve') ? (
                  <button
                    className="button"
                    disabled={!engine.info || busy}
                    onClick={() => {
                      setResolutionError('');
                      setResolution(instance.id);
                    }}
                  >
                    Resolve unknown outcome
                  </button>
                ) : null}
              </article>
            ))}
          </div>
        ) : (
          <pre className="json-view">
            {JSON.stringify(
              tab === 'inputs'
                ? { values: run.inputs, artifacts: run.inputArtifacts }
                : { values: run.outputs, artifacts: run.artifacts },
              null,
              2,
            )}
          </pre>
        )}
      </div>
      {resolution ? (
        <Modal
          title="Resolve unknown outcome"
          subtitle="Supply evidence about the external operation. The engine decides whether continuation is safe."
          onClose={() => setResolution('')}
        >
          <div className="modal-body">
            {resolutionError ? (
              <p className="form-error" role="alert">
                {resolutionError}
              </p>
            ) : null}
            <label className="field">
              Confirmed outcome
              <select
                aria-label="Confirmed outcome"
                value={outcome}
                onChange={(event) => setOutcome(event.target.value as typeof outcome)}
              >
                <option value="failed">Failed</option>
                <option value="not_started">Confirmed not started</option>
                <option value="succeeded">Succeeded with verified outputs</option>
              </select>
            </label>
            <label className="field">
              Evidence
              <textarea
                aria-label="Resolution evidence"
                value={evidence}
                onChange={(event) => setEvidence(event.target.value)}
              />
            </label>
            {outcome === 'succeeded' ? (
              <label className="field">
                Verified output ports
                <textarea
                  aria-label="Resolution outputs"
                  value={outputs}
                  onChange={(event) => setOutputs(event.target.value)}
                />
              </label>
            ) : null}
          </div>
          <div className="modal-footer">
            <button
              className="button primary"
              disabled={
                !engine.info ||
                busy ||
                !evidence.trim() ||
                engine.pending.some(
                  (item) => item.request.op === 'resolve' && item.request.instanceId === resolution,
                )
              }
              onClick={() => void resolve()}
            >
              Submit resolution
            </button>
          </div>
        </Modal>
      ) : null}
    </div>
  );
}
