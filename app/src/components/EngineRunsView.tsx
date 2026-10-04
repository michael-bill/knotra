import { useI18n } from '../lib/i18n';
import {
  executionActionLabels,
  executionConnectionLabels,
  executionStatusLabels,
  executionTabLabels,
} from '../lib/executionLabels';
import { useEffect, useState } from 'react';
import { ArrowLeft, Play, Search, XCircle } from 'lucide-react';
import { engineError, eventWindow, storedEvents, watchRun } from '../lib/engine/client';
import type { EngineEvent, EngineRun } from '../lib/engine/types';
import type { EngineController } from '../lib/engine/useEngine';
import { Empty, Modal, Status, time } from './ui';
import { Diagnostics } from './EngineDiagnostics';
import SourceEditor from './SourceEditor';
import EngineExecutionView from './EngineExecutionView';
import { EngineCompareView, EngineReplayView } from './EngineHistoryView';

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
  const { t, locale } = useI18n();
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
          <h1>{t('execution.engineRuns')}</h1>
          <p>{t('execution.executionStateBelongsToTheEngineClosingThis')}</p>
        </div>
      </header>
      <div className="list-toolbar runs-toolbar">
        <label className="search-field">
          <Search size={16} />
          <input
            aria-label={t('execution.searchEngineRuns')}
            placeholder={t('execution.searchRuns2')}
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
        </label>
        <label className="status-filter">
          <select
            aria-label={t('execution.filterEngineRuns')}
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
                {t(executionStatusLabels[status] ?? status)}
              </option>
            ))}
          </select>
        </label>
      </div>
      {filtered.length ? (
        <div className="run-table">
          <div className="table-header">
            <span>{t('execution.pipelineRun')}</span>
            <span>{t('execution.status')}</span>
            <span>{t('execution.started')}</span>
            <span>{t('execution.profile')}</span>
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
              <span className="small muted">{time(run.createdAt, locale)}</span>
              <span className="small">{run.profile}</span>
            </button>
          ))}
        </div>
      ) : (
        <Empty icon={<Play />} title={t('execution.noEngineRuns')}>
          {t('execution.startAWorkflowFromItsEditorAfterConnecting')}
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
  const { t, locale, message } = useI18n();
  const [tab, setTab] = useState('graph');
  const [events, setEvents] = useState<EngineEvent[]>([]);
  const [stream, setStream] = useState('Connecting…');
  const [resolution, setResolution] = useState('');
  const [evidence, setEvidence] = useState('');
  const [outcome, setOutcome] = useState<'succeeded' | 'not_started' | 'failed'>('failed');
  const [outputs, setOutputs] = useState('{}');
  const [resolutionError, setResolutionError] = useState('');
  const [localResolutionError, setLocalResolutionError] = useState(false);
  const [jsonError, setJsonError] = useState(false);
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
            eventWindow([
              ...events,
              ...current.filter((event) => !events.some((saved) => saved.id === event.id)),
            ]),
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
            : eventWindow([...events, message.event]),
        );
        // Token and tool observations update the inspector directly. Projection events
        // trigger authoritative snapshots without refetching every catalog on each token.
        const observation = /^(model|tool|agent|output)\./.test(message.event.type);
        if (!observation && !timer)
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
    setLocalResolutionError(false);
    setJsonError(false);
    let localFailure = false;
    try {
      const values = outcome === 'succeeded' ? JSON.parse(outputs) : {};
      if (!values || typeof values !== 'object' || Array.isArray(values)) {
        localFailure = true;
        throw new Error('Resolution outputs must be an object.');
      }
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
      setLocalResolutionError(localFailure);
      setJsonError(error instanceof SyntaxError);
    } finally {
      setBusy(false);
    }
  }
  const connectionStatus = stream.split(' · ')[0];
  const streamLabel =
    stream === 'Connecting…' || stream === 'Disconnected · cached run'
      ? message(stream)
      : ['connected', 'reconnecting'].includes(connectionStatus)
        ? `${t(executionConnectionLabels[connectionStatus] ?? connectionStatus)}${stream.slice(connectionStatus.length)}`
        : stream;
  return (
    <div className="run-detail">
      <div className="run-heading">
        <button className="text-button" onClick={onBack}>
          <ArrowLeft size={15} />
          {t('execution.allEngineRuns')}
        </button>
        <span className="type-label">{t('execution.engineRun')}</span>
      </div>
      <header className="page-heading">
        <div>
          <h1>{run.title}</h1>
          <p>
            {run.id} · {run.profile} · {time(run.createdAt, locale)}
          </p>
        </div>
        <div className="heading-actions">
          <Status status={run.status} />
          {engine.requests.some(
            (request) => request.runId === run.id && request.status === 'open',
          ) ? (
            <button className="button primary" onClick={onReview}>
              {t('execution.reviewRequest')}
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
                {t(executionActionLabels[action] ?? action)}
              </button>
            ))}
        </div>
      </header>
      <Diagnostics diagnostics={run.diagnostics} />
      <div className="underline-tabs">
        {[
          'graph',
          'replay',
          'compare',
          'instances',
          'timeline',
          'inputs',
          'outputs',
          'snapshot',
        ].map((tabName) => (
          <button
            key={tabName}
            className={tab === tabName ? 'active' : ''}
            onClick={() => setTab(tabName)}
          >
            {t(
              tabName === 'graph'
                ? 'observe.graph'
                : ['replay', 'compare'].includes(tabName)
                  ? `history.${tabName}`
                  : (executionTabLabels[tabName] ?? tabName),
            )}
          </button>
        ))}
      </div>
      <div className={`engine-run-content ${tab === 'graph' ? 'engine-observer-content' : ''}`}>
        {tab === 'graph' ? (
          <EngineExecutionView
            run={run}
            events={events}
            connection={streamLabel}
            connected={!!engine.info}
            onReview={onReview}
            onResolve={(id) => {
              setResolutionError('');
              setResolution(id);
            }}
          />
        ) : tab === 'replay' ? (
          <EngineReplayView run={run} connected={!!engine.info} onLive={() => setTab('graph')} />
        ) : tab === 'compare' ? (
          <EngineCompareView run={run} runs={engine.runs} connected={!!engine.info} />
        ) : tab === 'snapshot' ? (
          <SourceEditor
            source={run.package.source}
            readOnly
            label={t('execution.engineImmutableSnapshot')}
          />
        ) : tab === 'timeline' ? (
          <>
            <p className="small muted">{streamLabel}</p>
            <div className="timeline">
              {events.map((event) => (
                <div className="timeline-event" key={event.id}>
                  <span className="timeline-marker" />
                  <time>{time(event.at, locale)}</time>
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
                      {instance.scope || t('execution.rootGraph')} ·{' '}
                      {instance.attemptId ?? t('execution.noAttempt')}
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
                    {t('execution.resolveUnknownOutcome')}
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
          title={t('execution.resolveUnknownOutcome')}
          subtitle={t('execution.supplyEvidenceAboutTheExternalOperationTheEngine')}
          onClose={() => setResolution('')}
        >
          <div className="modal-body">
            {resolutionError ? (
              <p className="form-error" role="alert">
                {localResolutionError
                  ? t('execution.resolutionOutputsMustBeAnObject')
                  : jsonError
                    ? t('execution.invalidJsonMessage', { message: resolutionError })
                    : resolutionError}
              </p>
            ) : null}
            <label className="field">
              {t('execution.confirmedOutcome')}
              <select
                aria-label={t('execution.confirmedOutcome')}
                value={outcome}
                onChange={(event) => setOutcome(event.target.value as typeof outcome)}
              >
                <option value="failed">{t('execution.failed')}</option>
                <option value="not_started">{t('execution.confirmedNotStarted')}</option>
                <option value="succeeded">{t('execution.succeededWithVerifiedOutputs')}</option>
              </select>
            </label>
            <label className="field">
              {t('execution.evidence')}
              <textarea
                aria-label={t('execution.resolutionEvidence')}
                value={evidence}
                onChange={(event) => setEvidence(event.target.value)}
              />
            </label>
            {outcome === 'succeeded' ? (
              <label className="field">
                {t('execution.verifiedOutputPorts')}
                <textarea
                  aria-label={t('execution.resolutionOutputs')}
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
              {t('execution.submitResolution')}
            </button>
          </div>
        </Modal>
      ) : null}
    </div>
  );
}
