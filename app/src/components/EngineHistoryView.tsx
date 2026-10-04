import { useEffect, useMemo, useRef, useState } from 'react';
import { ArrowRight, History, RefreshCw, StepBack, StepForward } from 'lucide-react';
import { useI18n } from '../lib/i18n';
import { engineCall, engineError } from '../lib/engine/client';
import {
  appendReplayEvents,
  compareRuns,
  replayRun,
  type ValueChange,
} from '../lib/engine/history';
import {
  duration,
  durationLabel,
  executionGraphs,
  graphInstances,
  graphStatuses,
  packageGraphs,
} from '../lib/engine/observation';
import type { EngineEvent, EngineRun, Page } from '../lib/engine/types';
import GraphView from './GraphView';
import { Status, time } from './ui';
import './EngineHistoryView.css';

function Preview({ value }: { value: unknown }) {
  const { t } = useI18n();
  if (value === undefined) return <p className="muted">{t('history.absent')}</p>;
  const text = typeof value === 'string' ? value : JSON.stringify(value, null, 2);
  return (
    <>
      <pre className="history-value">{text.slice(0, 24 * 1024)}</pre>
      {text.length > 24 * 1024 && <p className="muted">{t('history.previewLimited')}</p>}
    </>
  );
}

export function EngineReplayView({
  run,
  connected,
  onLive,
}: {
  run: EngineRun;
  connected: boolean;
  onLive: () => void;
}) {
  const { t, locale } = useI18n();
  const [events, setEvents] = useState<EngineEvent[]>([]);
  const [cursor, setCursor] = useState<string | null>(null);
  const [complete, setComplete] = useState(false);
  const [limited, setLimited] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [count, setCount] = useState(0);
  const [scanned, setScanned] = useState(0);
  const [groupKey, setGroupKey] = useState('root');
  const [selected, setSelected] = useState('');
  const revision = useRef(0);
  const loading = useRef(false);
  const loadedRun = useRef('');
  async function load(reset: boolean) {
    if (!connected || loading.current) return;
    loading.current = true;
    const generation = revision.current;
    setBusy(true);
    setError('');
    let nextCursor = reset ? null : cursor;
    let nextEvents = reset ? [] : events;
    let inspected = reset ? 0 : scanned;
    let done = false,
      capped = false;
    try {
      // Bound each action's network work. The continuation remains explicit.
      for (let page = 0; page < 20; page++) {
        const result: Page<EngineEvent> = await engineCall({
          op: 'history',
          runId: run.id,
          cursor: nextCursor,
        });
        if (generation !== revision.current) return;
        inspected += result.items.length;
        const retained = appendReplayEvents(nextEvents, result.items);
        nextEvents = retained.events;
        capped = retained.limited;
        done = result.nextCursor === null;
        if (result.nextCursor && result.nextCursor === nextCursor)
          throw new Error(t('history.cursorDidNotAdvance'));
        nextCursor = result.nextCursor;
        if (done || capped) break;
      }
      setEvents(nextEvents);
      setCursor(nextCursor);
      setComplete(done);
      setLimited(capped);
      setScanned(inspected);
      if (reset) setCount(nextEvents.length);
    } catch (cause) {
      if (generation === revision.current) setError(engineError(cause).message);
    } finally {
      if (generation === revision.current) {
        loading.current = false;
        setBusy(false);
      }
    }
  }
  useEffect(() => {
    revision.current++;
    loading.current = false;
    const changedRun = loadedRun.current !== run.id;
    loadedRun.current = run.id;
    if (changedRun) {
      setEvents([]);
      setCursor(null);
      setComplete(false);
      setLimited(false);
      setCount(0);
      setScanned(0);
      setGroupKey('root');
      setSelected('');
    }
    setBusy(false);
    setError('');
    if (connected && (changedRun || events.length === 0)) void load(true);
    return () => {
      revision.current++;
      loading.current = false;
    };
    // A fresh authoritative snapshot must not reset a user's historical selection.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [run.id, connected]);
  const historical = useMemo(() => replayRun(run, events, count), [run, events, count]);
  const graphs = useMemo(() => packageGraphs(run), [run.package]);
  const groups = useMemo(() => executionGraphs(historical, graphs), [historical, graphs]);
  const group = groups.find((item) => item.key === groupKey) ?? groups[0];
  const instances = graphInstances(group);
  const selectedInstance = group?.key === groupKey ? instances[selected] : undefined;
  const current = events[count - 1];
  return (
    <section className="history-view" aria-label={t('history.replay')}>
      <div className="history-toolbar">
        <div>
          <strong>
            <History size={16} />
            {t('history.savedState')}
          </strong>
          <p>{t('history.replayExplanation')}</p>
        </div>
        <button className="button" onClick={onLive}>
          {t('history.returnLive')}
          <ArrowRight size={14} />
        </button>
      </div>
      <div className="history-transport">
        <button
          className="icon-button"
          aria-label={t('history.previous')}
          disabled={count === 0}
          onClick={() => setCount((value) => Math.max(0, value - 1))}
        >
          <StepBack size={17} />
        </button>
        <input
          type="range"
          min={0}
          max={events.length}
          step={1}
          value={count}
          onChange={(event) => setCount(Number(event.target.value))}
          aria-label={t('history.position')}
        />
        <button
          className="icon-button"
          aria-label={t('history.next')}
          disabled={count === events.length}
          onClick={() => setCount((value) => Math.min(events.length, value + 1))}
        >
          <StepForward size={17} />
        </button>
        <span>{t('history.transitionCount', { current: count, total: events.length })}</span>
        <Status status={historical.status} />
      </div>
      <div className="history-cursor">
        <span>
          {current
            ? `${time(current.at, locale)} · ${current.instanceId ? (historical.instances.find((item) => item.id === current.instanceId)?.nodeId ?? current.instanceId) : run.title} · ${current.message}`
            : t('history.accepted')}
        </span>
      </div>
      <div className="history-load">
        <span className="muted">
          {t('history.readCount', { count: scanned })} ·{' '}
          {t(complete ? 'history.loaded' : 'history.partial')}
        </span>
        <button
          className="button small"
          disabled={!connected || busy}
          onClick={() => void load(true)}
        >
          <RefreshCw size={13} />
          {t('history.reload')}
        </button>
        {!complete && !limited && (
          <button
            className="button small"
            disabled={!connected || busy}
            onClick={() => void load(false)}
          >
            {t(busy ? 'history.loading' : 'history.loadMore')}
          </button>
        )}
      </div>
      {!connected && <p className="history-notice">{t('history.connect')}</p>}
      {error && (
        <p className="history-notice" role="alert">
          {error}
        </p>
      )}
      {limited && <p className="history-notice">{t('history.limit')}</p>}
      {groups.length > 1 && (
        <label className="history-scope">
          {t('history.scope')}
          <select
            aria-label={t('history.scope')}
            value={group?.key}
            onChange={(event) => {
              setGroupKey(event.target.value);
              setSelected('');
            }}
          >
            {groups.map((item) => (
              <option value={item.key} key={item.key}>
                {item.key === 'root'
                  ? t('execution.rootGraph')
                  : `${historical.instances.find((node) => node.id === item.parentId)?.nodeId ?? item.scope} ${item.iterationIndex === undefined ? '' : `[${item.iterationIndex + 1}]`} · ${item.path || item.scope}`}
              </option>
            ))}
          </select>
        </label>
      )}
      <div className="history-workspace">
        <div className="history-graph">
          {group ? (
            <GraphView
              key={group.key}
              graph={group.graph}
              selected={group?.key === groupKey ? selected : undefined}
              onSelect={(id) => {
                if (group) setGroupKey(group.key);
                setSelected(id);
              }}
              statuses={graphStatuses(group)}
              importedGraphs={graphs}
              observations={Object.fromEntries(
                Object.entries(instances).map(([id, instance]) => [id, instance.reason ?? '']),
              )}
            />
          ) : (
            <p>{t('history.noGraph')}</p>
          )}
        </div>
        <aside className="history-inspector" aria-label={t('history.savedDetails')}>
          {selectedInstance ? (
            <>
              <div className="history-inspector-title">
                <strong>{selectedInstance.nodeId}</strong>
                <Status status={selectedInstance.status} />
              </div>
              {selectedInstance.reason && <p>{selectedInstance.reason}</p>}
              <p className="muted">
                {selectedInstance.scope} ·{' '}
                {durationLabel(
                  duration(
                    selectedInstance.startedAt,
                    selectedInstance.finishedAt ?? historical.updatedAt,
                  ),
                )}
              </p>
              {selectedInstance.dataTruncated && (
                <p className="history-notice">{t('history.contextLimited')}</p>
              )}
              <h4>{t('history.inputs')}</h4>
              <Preview value={selectedInstance.inputs} />
              <h4>{t('history.outputs')}</h4>
              <Preview value={selectedInstance.outputs} />
              {selectedInstance.error && (
                <p className="history-notice">{selectedInstance.error.message}</p>
              )}
            </>
          ) : (
            <>
              <h3>{selected ? selected : t('history.selectNode')}</h3>
              <p className="muted">
                {t(selected ? 'history.notStarted' : 'history.inspectorExplanation')}
              </p>
            </>
          )}
        </aside>
      </div>
    </section>
  );
}

function Changes({ label, changes }: { label: string; changes: ValueChange[] }) {
  const { t } = useI18n();
  const [limit, setLimit] = useState(30);
  return (
    <details className="history-change-section" aria-label={label} open={changes.length > 0}>
      <summary>
        {label}
        <span>{changes.length}</span>
      </summary>
      {!changes.length ? (
        <p className="muted">{t('history.unchanged')}</p>
      ) : (
        changes.slice(0, limit).map((change) => (
          <details className="history-value-change" key={change.key}>
            <summary>{change.key}</summary>
            <div className="history-diff">
              <div>
                <h4>{t('history.before')}</h4>
                <Preview value={change.before} />
              </div>
              <div>
                <h4>{t('history.after')}</h4>
                <Preview value={change.after} />
              </div>
            </div>
          </details>
        ))
      )}
      {changes.length > limit && (
        <button className="button small" onClick={() => setLimit((value) => value + 30)}>
          {t('history.showMore')}
        </button>
      )}
    </details>
  );
}

export function EngineCompareView({
  run,
  runs,
  connected,
}: {
  run: EngineRun;
  runs: EngineRun[];
  connected: boolean;
}) {
  const { t, locale } = useI18n();
  const candidates = useMemo(
    () =>
      runs
        .filter(
          (item) => item.id !== run.id && Date.parse(item.createdAt) <= Date.parse(run.createdAt),
        )
        .sort((a, b) => b.createdAt.localeCompare(a.createdAt)),
    [runs, run.id, run.createdAt],
  );
  const [selected, setSelected] = useState('');
  const [before, setBefore] = useState<EngineRun>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [limit, setLimit] = useState(100);
  useEffect(() => {
    setSelected('');
    setBefore(undefined);
    setError('');
    setLimit(100);
  }, [run.id]);
  useEffect(() => {
    let stopped = false;
    setError('');
    setBusy(false);
    if (!connected) return;
    setBefore(undefined);
    if (selected) {
      setBusy(true);
      void engineCall<{ run: EngineRun }>({ op: 'run', runId: selected })
        .then((result) => {
          if (!stopped) setBefore(result.run);
        })
        .catch((cause) => {
          if (!stopped) setError(engineError(cause).message);
        })
        .finally(() => {
          if (!stopped) setBusy(false);
        });
    }
    return () => {
      stopped = true;
    };
  }, [selected, connected]);
  const comparison = useMemo(() => (before ? compareRuns(before, run) : undefined), [before, run]);
  const rows = comparison?.instances ?? [];
  return (
    <section className="history-view" aria-label={t('history.compare')}>
      <div className="history-toolbar">
        <div>
          <strong>{t('history.compare')}</strong>
          <p>{t('history.compareExplanation')}</p>
        </div>
      </div>
      <label className="history-scope">
        {t('history.baseline')}
        <select
          aria-label={t('history.baseline')}
          value={selected}
          onChange={(event) => {
            setSelected(event.target.value);
            setLimit(100);
          }}
          disabled={!connected}
        >
          <option value="">{t('history.chooseRun')}</option>
          {candidates.map((item) => (
            <option key={item.id} value={item.id}>
              {item.title} · {time(item.createdAt, locale)} · {item.id.slice(0, 8)}
            </option>
          ))}
        </select>
      </label>
      {!connected && <p className="history-notice">{t('history.connect')}</p>}
      {!candidates.length && <p className="muted">{t('history.noEarlierRuns')}</p>}
      {busy && <p role="status">{t('history.loading')}</p>}
      {error && (
        <p className="history-notice" role="alert">
          {error}
        </p>
      )}
      {before && comparison && (
        <>
          <div className="history-comparison-heading">
            <div>
              <small>{t('history.before')}</small>
              <strong>{before.title}</strong>
              <span>{time(before.createdAt, locale)}</span>
              <Status status={before.status} />
            </div>
            <ArrowRight size={18} />
            <div>
              <small>{t('history.after')}</small>
              <strong>{run.title}</strong>
              <span>{time(run.createdAt, locale)}</span>
              <Status status={run.status} />
            </div>
          </div>
          <Changes label={t('history.prompts')} changes={comparison.prompts} />
          <Changes label={t('history.source')} changes={comparison.source} />
          <details className="history-change-section">
            <summary>
              {t('history.files')}
              <span>{comparison.files.length}</span>
            </summary>
            <p className="muted">{t('history.fileExplanation')}</p>
            {comparison.files.length ? (
              <ul>
                {comparison.files.map((file) => (
                  <li key={file.key}>
                    {file.key} ·{' '}
                    {t(
                      file.before === undefined
                        ? 'history.added'
                        : file.after === undefined
                          ? 'history.removed'
                          : 'history.modified',
                    )}
                  </li>
                ))}
              </ul>
            ) : (
              <p className="muted">{t('history.unchanged')}</p>
            )}
          </details>
          <Changes label={t('history.inputs')} changes={comparison.inputs} />
          <Changes label={t('history.inputArtifacts')} changes={comparison.inputArtifacts} />
          <Changes label={t('history.outputs')} changes={comparison.outputs} />
          <details className="history-change-section" open>
            <summary>
              {t('history.nodeComparison')}
              <span>
                {t('history.changedCount', {
                  count: rows.filter((row) => row.changed).length,
                  total: rows.length,
                })}
              </span>
            </summary>
            <p className="muted">{t('history.nodeExplanation')}</p>
            <div className="history-node-table">
              <div className="history-node-header">
                <span>{t('history.node')}</span>
                <span>{t('history.before')}</span>
                <span>{t('history.after')}</span>
              </div>
              {rows.slice(0, limit).map((row, index) => (
                <div
                  className={`history-node-row ${row.changed ? 'history-node-changed' : ''}`}
                  key={`${row.key}:${index}`}
                >
                  <div>
                    <strong>{row.after?.nodeId ?? row.before?.nodeId}</strong>
                    <small>{row.key}</small>
                    {row.ambiguous && <small>{t('history.ambiguous')}</small>}
                  </div>
                  <div>
                    {row.before ? (
                      <>
                        <Status status={row.before.status} />
                        <span>{durationLabel(row.beforeDuration)}</span>
                      </>
                    ) : (
                      '—'
                    )}
                  </div>
                  <div>
                    {row.after ? (
                      <>
                        <Status status={row.after.status} />
                        <span>{durationLabel(row.afterDuration)}</span>
                      </>
                    ) : (
                      '—'
                    )}
                  </div>
                </div>
              ))}
            </div>
            {rows.length > limit && (
              <button className="button small" onClick={() => setLimit((value) => value + 100)}>
                {t('history.showMore')}
              </button>
            )}
          </details>
        </>
      )}
    </section>
  );
}
