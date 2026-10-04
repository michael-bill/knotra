import { useEffect, useMemo, useRef, useState } from 'react';
import {
  Activity as ActivityIcon,
  AlertCircle,
  ArrowLeft,
  ArrowRight,
  Check,
  ChevronRight,
  Clock3,
  Download,
  FileBox,
  Focus,
  GitBranch,
  Layers3,
  Radio,
  X,
} from 'lucide-react';
import { dependencies } from '../lib/graph';
import { editorStatusKeys } from '../lib/editorLabels';
import { useI18n } from '../lib/i18n';
import { record, type NodeDefinition } from '../lib/types';
import { engineCall, engineError, exportEngineArtifact } from '../lib/engine/client';
import {
  activities,
  duration,
  durationLabel,
  executionGraphs,
  graphInstances,
  instanceArtifacts,
  graphStatuses,
  mergeEvents,
  packageGraphs,
  observationWindow,
  type Activity,
  type ExecutionGraph,
} from '../lib/engine/observation';
import {
  terminal,
  type EngineEvent,
  type EngineInstance,
  type EngineRun,
  type Page,
} from '../lib/engine/types';
import GraphView from './GraphView';
import { Diagnostics } from './EngineDiagnostics';
import { Empty, NodeIcon, size, Status, time } from './ui';

interface EdgeSelection {
  source: string;
  target: string;
  sourcePort?: string;
  targetPort?: string;
}

export default function EngineExecutionView({
  run,
  events,
  connection,
  connected,
  onReview,
  onResolve,
}: {
  run: EngineRun;
  events: EngineEvent[];
  connection: string;
  connected: boolean;
  onReview: () => void;
  onResolve: (id: string) => void;
}) {
  const { t, locale } = useI18n();
  const graphs = useMemo(() => packageGraphs(run), [run.package]);
  const groups = useMemo(
    () => executionGraphs(run, graphs),
    [run.instances, run.package.entrypoint, graphs],
  );
  const [groupKey, setGroupKey] = useState('root');
  const [selected, setSelected] = useState('');
  const [edge, setEdge] = useState<EdgeSelection>();
  const [follow, setFollow] = useState(false);
  const [showWaterfall, setShowWaterfall] = useState(false);
  const [now, setNow] = useState(Date.now());
  const group = groups.find((item) => item.key === groupKey) ?? groups[0];
  const instances = useMemo(() => graphInstances(group), [group]);
  const statuses = useMemo(() => (group ? graphStatuses(group) : {}), [group]);
  const selectedInstance = instances[selected];
  const node = group?.graph.nodes[selected];
  useEffect(() => {
    if (terminal(run.status)) return;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [run.status]);
  useEffect(() => {
    if (!follow) return;
    const active = [...run.instances]
      .reverse()
      .find((item) => ['running', 'waiting_human', 'waiting_resolution'].includes(item.status));
    const location =
      active && groups.find((item) => item.instances.some((instance) => instance.id === active.id));
    if (active && location) {
      setGroupKey(location.key);
      setSelected(active.nodeId);
      setEdge(undefined);
    }
  }, [follow, run.instances, groups]);
  const counts = run.instances.reduce<Record<string, number>>((counts, instance) => {
    counts[instance.status] = (counts[instance.status] ?? 0) + 1;
    return counts;
  }, {});
  function pick(id: string) {
    setSelected(id);
    setEdge(undefined);
    setFollow(false);
  }
  function pickInstance(instance: EngineInstance) {
    const location = groups.find((item) =>
      item.instances.some((entry) => entry.id === instance.id),
    );
    if (location) {
      setGroupKey(location.key);
      pick(instance.nodeId);
    }
  }
  function groupLabel(entry: ExecutionGraph) {
    if (entry.key === 'root') return t('execution.rootGraph');
    const labels: string[] = [];
    const visited = new Set<string>();
    let parentId = entry.parentId;
    let iteration = entry.iterationIndex;
    while (parentId && !visited.has(parentId)) {
      visited.add(parentId);
      const parent = run.instances.find((item) => item.id === parentId);
      if (!parent) break;
      labels.unshift(
        `${parent.nodeId}${iteration === undefined ? '' : ` · ${t('observe.iteration', { count: iteration + 1 })}`}`,
      );
      parentId = parent.parentInstanceId;
      iteration = parent.iterationIndex;
    }
    return labels.length ? labels.join(' / ') : entry.scope;
  }
  const observations = useMemo(
    () =>
      Object.fromEntries(
        Object.entries(instances).map(([id, instance]) => {
          const children = run.instances.filter((item) => item.parentInstanceId === instance.id);
          const blockers =
            instance.status === 'pending' && group?.graph.nodes[id]
              ? dependencies(group.graph.nodes[id]).filter(
                  (dependency) =>
                    !['succeeded', 'skipped'].includes(instances[dependency]?.status ?? 'pending'),
                )
              : [];
          const summary = children.length
            ? t('observe.childrenProgress', {
                done: children.filter((item) => terminal(item.status)).length,
                total: children.length,
              })
            : undefined;
          return [
            id,
            summary ??
              (blockers.length
                ? `${t('observe.waitsFor')}: ${blockers.join(', ')}`
                : instance.status === 'pending'
                  ? undefined
                  : instance.reason) ??
              durationLabel(
                duration(
                  instance.startedAt,
                  instance.finishedAt ??
                    (terminal(instance.status) ? (instance.updatedAt ?? run.updatedAt) : now),
                ),
              ),
          ];
        }),
      ),
    [instances, group, run.instances, run.updatedAt, now, t],
  );
  const latest = events.at(-1)?.at;
  const unresolved = run.availableActions.includes('resolve')
    ? run.instances.filter((instance) => instance.status === 'waiting_resolution')
    : [];
  return (
    <div className="execution-observer">
      <div className="execution-summary">
        <div className="execution-counts">
          <span className="execution-summary-label">
            <ActivityIcon size={15} />
            {t('observe.execution')}
          </span>
          <span>
            <i className="execution-dot state-succeeded" />
            {t('observe.doneCount', { count: counts.succeeded ?? 0 })}
          </span>
          <span>
            <i className="execution-dot state-running" />
            {t('observe.runningCount', { count: counts.running ?? 0 })}
          </span>
          <span>
            <i className="execution-dot state-waiting_human" />
            {t('observe.waitingCount', {
              count:
                (counts.waiting_human ?? 0) +
                (counts.waiting_resolution ?? 0) +
                (counts.retry_wait ?? 0),
            })}
          </span>
          {counts.failed ? (
            <span className="form-error">{t('observe.failedCount', { count: counts.failed })}</span>
          ) : null}
          {counts.skipped ? (
            <span>{t('observe.skippedCount', { count: counts.skipped })}</span>
          ) : null}
        </div>
        <span
          className={`execution-connection ${connection === t('execution.connected') ? 'is-connected' : ''}`}
          title={latest ? `${t('observe.lastEvent')}: ${time(latest, locale)}` : connection}
        >
          <Radio size={13} />
          {connection}
        </span>
      </div>
      {unresolved.length ? (
        <section className="execution-attention" aria-label={t('execution.waitingResolution')}>
          {unresolved.map((instance) => (
            <div key={instance.id}>
              <AlertCircle size={16} />
              <span>
                <strong>{instance.nodeId}</strong>
                <small>{t('execution.waitingResolution')}</small>
              </span>
              <button
                className="button"
                disabled={!connected}
                onClick={() => onResolve(instance.id)}
              >
                {t('execution.resolveUnknownOutcome')}
              </button>
            </div>
          ))}
        </section>
      ) : null}
      <div className="execution-toolbar">
        <label className="execution-scope-picker">
          <Layers3 size={15} />
          <select
            aria-label={t('observe.graphScope')}
            value={group?.key ?? ''}
            onChange={(event) => {
              setGroupKey(event.target.value);
              setSelected('');
              setEdge(undefined);
              setFollow(false);
            }}
          >
            {groups.map((entry) => (
              <option key={entry.key} value={entry.key}>
                {groupLabel(entry)}
              </option>
            ))}
          </select>
        </label>
        <div className="execution-toolbar-actions">
          <button
            className={`button small ${follow ? 'active' : ''}`}
            aria-pressed={follow}
            onClick={() => setFollow(!follow)}
          >
            <Focus size={14} />
            {t('observe.follow')}
          </button>
          <button
            className={`button small ${showWaterfall ? 'active' : ''}`}
            aria-pressed={showWaterfall}
            onClick={() => setShowWaterfall(!showWaterfall)}
          >
            <Clock3 size={14} />
            {t('observe.timing')}
          </button>
        </div>
      </div>
      <div className={`execution-workspace ${selected || edge ? 'has-inspector' : ''}`}>
        <div className="execution-canvas">
          {group ? (
            <GraphView
              key={group.key}
              graph={group.graph}
              importedGraphs={graphs}
              selected={selected}
              statuses={statuses}
              observations={observations}
              onSelect={pick}
              onSelectEdge={(value) => {
                setEdge(value);
                setSelected('');
                setFollow(false);
              }}
              onOpenBody={(id) => {
                const child = groups.find((entry) => entry.parentId === instances[id]?.id);
                if (child) {
                  setGroupKey(child.key);
                  setSelected('');
                  setFollow(false);
                }
              }}
            />
          ) : (
            <Empty icon={<GitBranch />} title={t('observe.noGraph')}>
              {t('observe.noGraphDetail')}
            </Empty>
          )}
          {!selected && !edge && group ? (
            <div className="execution-canvas-hint">
              <Focus size={14} />
              {t('observe.selectHint')}
            </div>
          ) : null}
        </div>
        {node && group ? (
          <InstanceInspector
            key={selectedInstance?.id ?? `${group.key}:${selected}`}
            run={run}
            instance={selectedInstance}
            node={node}
            nodeId={selected}
            group={group}
            groups={groups}
            events={events}
            connected={connected}
            follow={follow}
            now={now}
            onClose={() => {
              setSelected('');
              setFollow(false);
            }}
            onReview={onReview}
            onSelectInstance={pickInstance}
            onSelectNode={pick}
          />
        ) : null}
        {edge && group ? (
          <aside className="execution-inspector" aria-label={t('observe.edgeData')}>
            <header className="execution-inspector-heading">
              <div>
                <small>{t('observe.edgeData')}</small>
                <h2>
                  {edge.source}
                  <ArrowRight size={14} />
                  {edge.target}
                </h2>
              </div>
              <button
                className="icon-button"
                aria-label={t('observe.close')}
                onClick={() => setEdge(undefined)}
              >
                <X size={16} />
              </button>
            </header>
            <div className="execution-inspector-body">
              <p className="muted small">
                {edge.sourcePort
                  ? `${edge.source}.${edge.sourcePort} → ${edge.target}.${edge.targetPort}`
                  : t('observe.dependencyEdge')}
              </p>
              <DataBlock
                label={t('observe.sentValue')}
                value={
                  edge.sourcePort
                    ? portValue(instances[edge.source]?.outputs, edge.sourcePort)
                    : instances[edge.source]?.outputs
                }
                open
              />
              {edge.targetPort ? (
                <DataBlock
                  label={t('observe.receivedValue')}
                  value={portValue(instances[edge.target]?.inputs, edge.targetPort)}
                  open
                />
              ) : null}
              <button className="button" onClick={() => pick(edge.source)}>
                {t('observe.openProducer')}
                <ChevronRight size={14} />
              </button>
            </div>
          </aside>
        ) : null}
      </div>
      {showWaterfall ? (
        <div className="execution-waterfall">
          <div className="execution-waterfall-heading">
            <strong>{t('observe.timing')}</strong>
            <span>
              {durationLabel(duration(run.createdAt, terminal(run.status) ? run.updatedAt : now))}
            </span>
          </div>
          <div className="execution-waterfall-rows">
            {run.instances
              .filter((instance) => instance.startedAt)
              .map((instance) => {
                const total = Math.max(
                  1,
                  duration(run.createdAt, terminal(run.status) ? run.updatedAt : now) ?? 1,
                );
                const start = duration(run.createdAt, instance.startedAt) ?? 0;
                const length =
                  duration(
                    instance.startedAt,
                    instance.finishedAt ??
                      (terminal(instance.status) ? (instance.updatedAt ?? run.updatedAt) : now),
                  ) ?? 0;
                return (
                  <button
                    key={instance.id}
                    className={`execution-waterfall-row ${instance.id === selectedInstance?.id ? 'active' : ''}`}
                    onClick={() => pickInstance(instance)}
                    title={`${instance.nodeId} · ${instance.id}`}
                  >
                    <span>
                      {instance.nodeId}
                      {instance.iterationIndex === undefined
                        ? ''
                        : ` [${instance.iterationIndex + 1}]`}
                    </span>
                    <div className="execution-waterfall-track">
                      <i
                        className={`state-${instance.status}`}
                        style={{
                          left: `${Math.min(99, (start / total) * 100)}%`,
                          width: `${Math.max(1, Math.min(100 - (start / total) * 100, (length / total) * 100))}%`,
                        }}
                      />
                    </div>
                    <small>{durationLabel(length)}</small>
                  </button>
                );
              })}
            {!run.instances.some((instance) => instance.startedAt) ? (
              <p className="muted small">{t('observe.noTiming')}</p>
            ) : null}
          </div>
        </div>
      ) : null}
    </div>
  );
}

function InstanceInspector({
  run,
  instance,
  node,
  nodeId,
  group,
  groups,
  events,
  connected,
  follow,
  now,
  onClose,
  onReview,
  onSelectInstance,
  onSelectNode,
}: {
  run: EngineRun;
  instance?: EngineInstance;
  node: NodeDefinition;
  nodeId: string;
  group: ExecutionGraph;
  groups: ExecutionGraph[];
  events: EngineEvent[];
  connected: boolean;
  follow: boolean;
  now: number;
  onClose: () => void;
  onReview: () => void;
  onSelectInstance: (instance: EngineInstance) => void;
  onSelectNode: (id: string) => void;
}) {
  const { t, locale } = useI18n();
  const [tab, setTab] = useState('activity');
  const [history, setHistory] = useState<EngineEvent[]>([]);
  const [cursor, setCursor] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [historyError, setHistoryError] = useState('');
  const [attempt, setAttempt] = useState('all');
  const [visibleRows, setVisibleRows] = useState(150);
  const [historyLimited, setHistoryLimited] = useState(false);
  const [showStateHistory, setShowStateHistory] = useState(false);
  const inspectorBody = useRef<HTMLDivElement>(null);
  const activityList = useRef<HTMLDivElement>(null);
  const id = instance?.id;
  useEffect(() => {
    if (!id || !connected) return;
    let active = true;
    setLoading(true);
    void (async () => {
      let next: string | null = null;
      let loaded: EngineEvent[] = [];
      try {
        // Retrieve ordinary runs completely; larger histories remain explicitly paginated.
        for (let pageNumber = 0; pageNumber < 10; pageNumber++) {
          const page: Page<EngineEvent> = await engineCall({
            op: 'history',
            runId: run.id,
            instanceId: id,
            cursor: next,
          });
          if (!active) return;
          loaded = mergeEvents(loaded, page.items);
          next = page.nextCursor;
          setHistory((current) => observationWindow(mergeEvents(current, page.items)));
          setCursor(next);
          setHistoryError('');
          if (!next || loaded.length >= 4000 || JSON.stringify(loaded).length > 3 * 1024 * 1024)
            break;
        }
      } catch (error) {
        if (active) setHistoryError(engineError(error).message);
      } finally {
        if (active) setLoading(false);
      }
    })();
    return () => {
      active = false;
    };
  }, [id, run.id, connected]);
  useEffect(() => {
    if (!id) return;
    const incoming = events.filter((event) => event.instanceId === id);
    if (!incoming.length) return;
    setHistory((current) => {
      const merged = mergeEvents(current, incoming);
      const bounded = observationWindow(merged);
      if (bounded.length !== merged.length) setHistoryLimited(true);
      return bounded;
    });
  }, [events, id]);
  async function loadMore() {
    if (!id || !cursor || loading) return;
    setLoading(true);
    try {
      const page = await engineCall<Page<EngineEvent>>({
        op: 'history',
        runId: run.id,
        instanceId: id,
        cursor,
      });
      setHistory((current) => {
        const merged = mergeEvents(current, page.items);
        const bounded = observationWindow(merged);
        if (bounded.length !== merged.length) setHistoryLimited(true);
        return bounded;
      });
      setCursor(page.nextCursor);
      setHistoryError('');
    } catch (error) {
      setHistoryError(engineError(error).message);
    } finally {
      setLoading(false);
    }
  }
  const ownEvents = useMemo(
    () =>
      mergeEvents(
        history,
        events.filter((event) => id && event.instanceId === id),
      ),
    [history, events, id],
  );
  const attempts = [
    ...new Set(ownEvents.flatMap((event) => (event.attemptId ? [event.attemptId] : []))),
  ];
  const rows = useMemo(
    () => activities(ownEvents.filter((event) => attempt === 'all' || event.attemptId === attempt)),
    [ownEvents, attempt],
  );
  const [activityRows, stateRows] = useMemo(() => {
    if (!['llm', 'agent'].includes(node.type) || !rows.some((row) => row.kind !== 'event'))
      return [rows, []];
    const routine = (row: Activity) =>
      row.kind === 'event' &&
      row.events[0].type === 'node' &&
      ['pending', 'ready', 'running', 'succeeded'].includes(
        String(record(row.events[0].data).status ?? row.events[0].message),
      );
    return [rows.filter((row) => !routine(row)), rows.filter(routine)];
  }, [rows, node.type]);
  const children = run.instances.filter((item) => item.parentInstanceId === id && id);
  const blockers = dependencies(node).filter(
    (dependency) =>
      !['succeeded', 'skipped'].includes(graphInstances(group)[dependency]?.status ?? 'pending'),
  );
  const status = instance?.status ?? 'pending';
  const parent = run.instances.find((item) => item.id === instance?.parentInstanceId);
  const artifacts = instanceArtifacts(run, instance);
  const latest = ownEvents.at(-1);
  useEffect(() => {
    if (!follow || tab !== 'activity') return;
    const frame = requestAnimationFrame(() => {
      const body = inspectorBody.current;
      const list = activityList.current;
      if (!body || !list) return;
      const responses = list.querySelectorAll<HTMLElement>('.execution-response pre');
      const response = responses[responses.length - 1];
      if (response) response.scrollTop = response.scrollHeight;
      // Scroll only this inspector, leaving the graph and the application viewport fixed.
      body.scrollTop +=
        list.getBoundingClientRect().bottom - body.getBoundingClientRect().bottom + 12;
    });
    return () => cancelAnimationFrame(frame);
  }, [follow, tab, latest?.id, activityRows.length]);
  const activeOperation = rows
    .filter(
      (row) => row.kind !== 'event' && !row.completed && row.attemptId === instance?.attemptId,
    )
    .at(-1);
  const config = record(node[node.type]);
  return (
    <aside className="execution-inspector" aria-label={t('observe.nodeDetails')}>
      <header className="execution-inspector-heading">
        <div>
          <small>
            <NodeIcon kind={node.type} size={13} />
            {node.type}
          </small>
          <h2>{nodeId}</h2>
        </div>
        <button className="icon-button" aria-label={t('observe.close')} onClick={onClose}>
          <X size={16} />
        </button>
      </header>
      <div className="execution-node-meta">
        <Status status={status} />
        <span>
          <Clock3 size={12} />
          {durationLabel(
            duration(
              instance?.startedAt,
              instance?.finishedAt ??
                (terminal(status) ? (instance?.updatedAt ?? run.updatedAt) : now),
            ),
          )}
        </span>
      </div>
      {parent ? (
        <button className="execution-parent-link" onClick={() => onSelectInstance(parent)}>
          <ArrowLeft size={12} />
          {parent.nodeId}
          {instance?.iterationIndex === undefined
            ? ''
            : ` · ${t('observe.iteration', { count: instance.iterationIndex + 1 })}`}
        </button>
      ) : null}
      <div className="execution-detail-tabs">
        {['activity', 'data', 'definition'].map((value) => (
          <button
            key={value}
            className={tab === value ? 'active' : ''}
            onClick={() => setTab(value)}
          >
            {t(`observe.${value}`)}
          </button>
        ))}
      </div>
      <div className="execution-inspector-body" ref={inspectorBody}>
        {instance?.error ? <Diagnostics diagnostics={[instance.error]} /> : null}
        {instance?.reason && status !== 'pending' ? (
          <p className="execution-wait-reason">{instance.reason}</p>
        ) : null}
        {status === 'pending' && blockers.length ? (
          <div className="execution-wait-reason">
            {t('observe.waitsFor')}
            <div>
              {blockers.map((dependency) => (
                <button
                  className="text-button"
                  key={dependency}
                  onClick={() => onSelectNode(dependency)}
                >
                  {dependency}
                  <ChevronRight size={12} />
                </button>
              ))}
            </div>
          </div>
        ) : null}
        {status === 'waiting_human' ? (
          <button className="button primary" onClick={onReview}>
            {t('execution.reviewRequest')}
          </button>
        ) : null}
        {tab === 'activity' ? (
          <>
            {['llm', 'agent'].includes(node.type) ? (
              <div className="execution-model-summary">
                <span>{String(config.model ?? '')}</span>
                {node.type === 'agent' ? (
                  <span>
                    {t('observe.agentStep', {
                      step: Math.max(
                        0,
                        ...ownEvents
                          .filter(
                            (event) =>
                              event.attemptId ===
                              (attempt === 'all' ? instance?.attemptId : attempt),
                          )
                          .map((event) => Number(record(event.data).step) || 0),
                      ),
                      max: String(config.maxSteps ?? '—'),
                    })}
                  </span>
                ) : null}
              </div>
            ) : null}
            {status === 'running' ? (
              <div className="execution-current">
                <span className="execution-live-dot" />
                <div>
                  <small>{t('observe.currentOperation')}</small>
                  <strong>
                    {activeOperation
                      ? t(
                          activeOperation.kind === 'model'
                            ? 'observe.generating'
                            : 'observe.toolRunning',
                        )
                      : latest
                        ? eventLabel(latest, t)
                        : t('observe.executing')}
                  </strong>
                </div>
              </div>
            ) : null}
            {attempts.length > 1 ? (
              <label className="execution-attempt-picker">
                {t('observe.attempt')}
                <select value={attempt} onChange={(event) => setAttempt(event.target.value)}>
                  <option value="all">{t('observe.allAttempts')}</option>
                  {attempts.map((value, index) => (
                    <option key={value} value={value}>
                      {index + 1} · {value}
                    </option>
                  ))}
                </select>
              </label>
            ) : null}
            {loading ? (
              <p className="small muted" role="status">
                {t('observe.loadingHistory')}
              </p>
            ) : null}
            {historyError ? (
              <p className="small muted">
                {t('observe.historyUnavailable')} {historyError}
              </p>
            ) : null}
            {cursor || historyLimited ? (
              <p className="execution-wait-reason">
                {t(historyLimited ? 'observe.historyLimited' : 'observe.historyPartial')}
              </p>
            ) : null}
            {cursor ? (
              <button
                className="button small"
                disabled={loading || !connected}
                onClick={() => void loadMore()}
              >
                {t('observe.loadMore')}
              </button>
            ) : null}
            <div className="execution-activity-list" ref={activityList}>
              {activityRows.slice(-visibleRows).map((row) => (
                <ActivityCard
                  key={row.id}
                  row={row}
                  running={
                    status === 'running' &&
                    (!row.attemptId || row.attemptId === instance?.attemptId)
                  }
                />
              ))}
            </div>
            {stateRows.length ? (
              <details
                className="execution-state-history"
                onToggle={(event) => setShowStateHistory(event.currentTarget.open)}
              >
                <summary>{t('observe.stateHistory', { count: stateRows.length })}</summary>
                {showStateHistory
                  ? stateRows
                      .slice(-visibleRows)
                      .map((row) => <ActivityCard key={row.id} row={row} running={false} />)
                  : null}
              </details>
            ) : null}
            {activityRows.length > visibleRows || stateRows.length > visibleRows ? (
              <button
                className="button small"
                onClick={() => setVisibleRows((count) => count + 150)}
              >
                {t('observe.showEarlier')}
              </button>
            ) : null}
            {!rows.length && !loading ? (
              <div className="execution-empty-detail">
                <ActivityIcon size={24} />
                <p>{t(instance ? 'observe.noActivity' : 'observe.notStarted')}</p>
              </div>
            ) : null}
            {children.length ? (
              <div className="execution-children">
                <h3>{t('observe.childInstances')}</h3>
                {children.map((child) => (
                  <button
                    key={child.id}
                    onClick={() => onSelectInstance(child)}
                    disabled={
                      !groups.some((entry) => entry.instances.some((item) => item.id === child.id))
                    }
                  >
                    <span>
                      {child.nodeId}
                      <small>
                        {child.iterationIndex === undefined
                          ? child.scope
                          : t('observe.iteration', { count: child.iterationIndex + 1 })}
                      </small>
                    </span>
                    <Status status={child.status} />
                    <ChevronRight size={14} />
                  </button>
                ))}
              </div>
            ) : null}
          </>
        ) : tab === 'data' ? (
          <>
            {instance?.dataTruncated ? (
              <p className="execution-wait-reason">{t('observe.dataTruncated')}</p>
            ) : null}
            <DataBlock label={t('execution.inputs')} value={instance?.inputs} open />
            <DataBlock label={t('execution.outputs')} value={instance?.outputs} open />
            {node.type === 'switch' ? (
              <DataBlock label={t('observe.routingConditions')} value={node.switch} open />
            ) : null}
            {artifacts.length ? (
              <section className="execution-artifacts">
                <h3>{t('observe.artifacts')}</h3>
                {artifacts.map((artifact) => (
                  <ArtifactCard key={artifact.id} artifact={artifact} connected={connected} />
                ))}
              </section>
            ) : null}
            <DataBlock
              label={t('observe.runtimeIdentity')}
              value={
                instance
                  ? {
                      id: instance.id,
                      scope: instance.scope,
                      graphPath: instance.graphPath,
                      attemptId: instance.attemptId,
                      startedAt: instance.startedAt,
                      finishedAt: instance.finishedAt,
                    }
                  : undefined
              }
            />
          </>
        ) : (
          <>
            <p className="small muted">{t('observe.immutableDefinition')}</p>
            <DataBlock label={nodeId} value={node} open />
          </>
        )}
        {latest ? (
          <p className="execution-last-event">
            {t('observe.lastEvent')} · {time(latest.at, locale)}
          </p>
        ) : null}
      </div>
    </aside>
  );
}

function ActivityCard({ row, running }: { row: Activity; running: boolean }) {
  const { t, locale } = useI18n();
  const [open, setOpen] = useState(false);
  if (row.kind === 'event') {
    const event = row.events[0];
    return (
      <div className="execution-event">
        <i />
        <div>
          <strong>{eventLabel(event, t)}</strong>
          <small>{time(event.at, locale)}</small>
          <DataBlock
            label={t('observe.details')}
            value={{ type: event.type, ...record(event.data) }}
          />
        </div>
      </div>
    );
  }
  const start = record(row.started?.data);
  const end = record(row.completed?.data);
  const finished = !!row.completed;
  const usage = record(end.usage);
  const inputTokens = end.inputTokens ?? usage.inputTokens ?? usage.promptTokens;
  const outputTokens = end.outputTokens ?? usage.outputTokens ?? usage.completionTokens;
  const failed =
    row.completed?.type === 'model.failed' ||
    end.isError === true ||
    record(end.result).isError === true;
  const error = end.error ?? record(end.result).error;
  const incomplete = row.observationIncomplete;
  const truncated = row.truncated;
  const active = !finished && running;
  return (
    <article
      className={`execution-operation ${active ? 'is-active' : ''} ${failed ? 'is-failed' : ''}`}
    >
      <button
        className="execution-operation-heading"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        <span className="execution-operation-icon">
          {failed ? (
            <AlertCircle size={14} />
          ) : finished ? (
            <Check size={14} />
          ) : (
            <ActivityIcon size={14} />
          )}
        </span>
        <span>
          <strong>
            {row.kind === 'model'
              ? t('observe.modelCall')
              : String(start.name ?? end.name ?? t('observe.toolCall'))}
          </strong>
          <small>
            {row.step ? `${t('observe.iteration', { count: row.step })} · ` : ''}
            {String(start.model ?? end.model ?? time(row.at, locale))}
          </small>
        </span>
        <span className="execution-operation-time">
          {finished
            ? durationLabel(
                typeof end.durationMs === 'number'
                  ? end.durationMs
                  : duration(row.at, row.completed?.at),
              )
            : active
              ? t('observe.live')
              : t('observe.incomplete')}
        </span>
        <ChevronRight className={open ? 'expanded' : ''} size={13} />
      </button>
      {row.kind === 'model' && (row.text || active) ? (
        <div className="execution-response">
          <span className="execution-response-label">
            {t(finished && !failed ? 'observe.modelResponse' : 'observe.partialResponse')}
          </span>
          <pre>
            {row.text || t('observe.waitingForText')}
            {active && row.text ? <span className="execution-text-cursor" /> : null}
          </pre>
        </div>
      ) : null}
      {failed ? (
        <p className="execution-operation-error">
          {t('observe.operationFailed')}
          {error ? `: ${typeof error === 'string' ? error : JSON.stringify(error)}` : ''}
        </p>
      ) : null}
      {truncated || incomplete ? (
        <p className="execution-observation-notice">
          {t(incomplete ? 'observe.observationIncomplete' : 'observe.observationTruncated')}
        </p>
      ) : null}
      {inputTokens !== undefined ||
      outputTokens !== undefined ||
      typeof end.firstTokenMs === 'number' ? (
        <div className="execution-usage">
          {typeof end.firstTokenMs === 'number' ? (
            <span>
              {t('observe.firstText')} {durationLabel(end.firstTokenMs)}
            </span>
          ) : null}
          {inputTokens !== undefined || outputTokens !== undefined ? (
            <span>
              {t('observe.tokens')} {String(inputTokens ?? '—')} → {String(outputTokens ?? '—')}
            </span>
          ) : null}
        </div>
      ) : null}
      {open ? (
        <div className="execution-operation-details">
          <DataBlock
            label={row.kind === 'model' ? t('observe.promptContext') : t('observe.arguments')}
            value={row.kind === 'model' ? (start.messages ?? start) : (start.arguments ?? start)}
            open
          />
          {finished ? <DataBlock label={t('observe.resultDetails')} value={end} /> : null}
        </div>
      ) : null}
    </article>
  );
}

function DataBlock({
  label,
  value,
  open = false,
}: {
  label: string;
  value: unknown;
  open?: boolean;
}) {
  const { t } = useI18n();
  const [expanded, setExpanded] = useState(open);
  const [full, setFull] = useState(false);
  const text = useMemo(
    () =>
      expanded && value !== undefined
        ? typeof value === 'string'
          ? value
          : JSON.stringify(value, null, 2)
        : '',
    [expanded, value],
  );
  return (
    <section className="execution-data-block">
      <button aria-expanded={expanded} onClick={() => setExpanded(!expanded)}>
        <ChevronRight size={13} className={expanded ? 'expanded' : ''} />
        <strong>{label}</strong>
      </button>
      {expanded ? (
        value === undefined ? (
          <p className="small muted">{t('observe.noData')}</p>
        ) : (
          <>
            <pre>{full ? text : text.slice(0, 12000)}</pre>
            {text.length > 12000 && !full ? (
              <button className="text-button" onClick={() => setFull(true)}>
                {t('observe.showFullValue')}
              </button>
            ) : null}
          </>
        )
      ) : null}
    </section>
  );
}

function ArtifactCard({
  artifact,
  connected,
}: {
  artifact: EngineRun['artifacts'][number];
  connected: boolean;
}) {
  const { t, locale } = useI18n();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  return (
    <div className="execution-artifact">
      <FileBox size={16} />
      <span>
        <strong>{artifact.name}</strong>
        <small>
          {artifact.mediaType} · {size(artifact.size, locale)}
        </small>
        {error ? <small className="form-error">{error}</small> : null}
      </span>
      <button
        className="icon-button"
        disabled={!connected || busy}
        aria-label={t('observe.downloadArtifact', { name: artifact.name })}
        onClick={async () => {
          setBusy(true);
          setError('');
          try {
            await exportEngineArtifact(artifact.id);
          } catch (error) {
            setError(engineError(error).message);
          } finally {
            setBusy(false);
          }
        }}
      >
        <Download size={15} />
      </button>
    </div>
  );
}

function portValue(envelope: EngineInstance['inputs'], port: string): unknown {
  return envelope && Object.hasOwn(envelope.values ?? {}, port)
    ? envelope.values[port]
    : envelope?.artifacts?.[port];
}

function eventLabel(event: EngineEvent, t: ReturnType<typeof useI18n>['t']): string {
  if (event.type === 'agent.iteration')
    return t('observe.iteration', { count: Number(record(event.data).step) || 1 });
  if (
    (event.type === 'node' || event.type === 'run') &&
    Object.hasOwn(editorStatusKeys, event.message)
  )
    return t(editorStatusKeys[event.message as keyof typeof editorStatusKeys]);
  if (event.type === 'output.validating') return t('observe.validatingOutputs');
  if (event.type === 'output.completed')
    return t(
      record(event.data).valid === true
        ? 'observe.outputsVerified'
        : 'observe.outputValidationFailed',
    );
  return event.message || event.type;
}
