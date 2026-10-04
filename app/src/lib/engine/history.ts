import { decodeText } from '../bytes';
import { record, type Graph, type Json } from '../types';
import { duration, mergeEvents, packageGraphs } from './observation';
import {
  terminal,
  type EngineEvent,
  type EngineInstance,
  type EngineRun,
  type EngineStatus,
} from './types';

const statuses = new Set<EngineStatus>([
  'pending',
  'ready',
  'running',
  'retry_wait',
  'waiting_human',
  'waiting_resolution',
  'succeeded',
  'skipped',
  'failed',
  'cancelled',
]);
const text = (value: unknown): string | undefined =>
  typeof value === 'string' ? value : undefined;

// Keep a prefix from the beginning. A suffix cannot reconstruct earlier states faithfully.
export function appendReplayEvents(
  previous: EngineEvent[],
  incoming: EngineEvent[],
  maxBytes = 8 * 1024 * 1024,
  maxEvents = 5000,
) {
  const events: EngineEvent[] = [];
  let bytes = 0;
  for (const event of mergeEvents(
    previous,
    incoming.filter((event) => event.type === 'node' || event.type === 'run'),
  )) {
    const size = new TextEncoder().encode(JSON.stringify(event)).byteLength;
    if (events.length >= maxEvents || bytes + size > maxBytes) return { events, limited: true };
    events.push(event);
    bytes += size;
  }
  return { events, limited: false };
}

function context(value: unknown): EngineInstance['inputs'] {
  const envelope = record(value);
  if (!Object.hasOwn(envelope, 'values') || !Object.hasOwn(envelope, 'artifacts')) return;
  return {
    values: record(envelope.values) as Record<string, Json>,
    artifacts: record(envelope.artifacts) as Record<string, Json>,
  };
}

// The immutable package and accepted inputs are available throughout a run. Every
// mutable field below comes only from events at/before the chosen cursor; the
// latest snapshot must never leak a future result into the historical inspector.
export function replayRun(run: EngineRun, events: EngineEvent[], count: number): EngineRun {
  const historical: EngineRun = {
    ...run,
    status: 'pending',
    updatedAt: run.createdAt,
    outputs: {},
    artifacts: [],
    instances: [],
    diagnostics: [],
    availableActions: [],
  };
  const instances = new Map<string, EngineInstance>();
  const identity = new Map(run.instances.map((instance) => [instance.id, instance]));
  for (const event of events.slice(0, Math.max(0, count))) {
    if (event.runId !== run.id || (event.type !== 'node' && event.type !== 'run')) continue;
    const data = record(event.data);
    const status = text(data.status) ?? event.message;
    if (!statuses.has(status as EngineStatus)) continue;
    historical.updatedAt = event.at;
    if (event.type === 'run') {
      historical.status = status as EngineStatus;
      historical.outputs = context(data.outputs)?.values ?? historical.outputs;
      continue;
    }
    if (!event.instanceId) continue;
    const known = identity.get(event.instanceId);
    const previous = instances.get(event.instanceId);
    const next: EngineInstance = {
      id: event.instanceId,
      nodeId: text(data.nodeId) || known?.nodeId || event.instanceId,
      scope: text(data.scope) || known?.scope || run.package.entrypoint,
      status: status as EngineStatus,
      // Identity is immutable. Legacy events may only contain instance IDs.
      parentInstanceId: text(data.parentInstanceId) || known?.parentInstanceId,
      graphPath: text(data.graphPath) || known?.graphPath,
      nodeType: text(data.nodeType) || known?.nodeType,
      iterationIndex:
        typeof data.iterationIndex === 'number' ? data.iterationIndex : known?.iterationIndex,
      attemptId: event.attemptId ?? previous?.attemptId,
      inputs: context(data.inputs) ?? previous?.inputs,
      outputs: context(data.outputs),
      dataTruncated: data.dataTruncated === true || previous?.dataTruncated,
      startedAt: text(data.startedAt) ?? previous?.startedAt,
      finishedAt: text(data.finishedAt),
      updatedAt: event.at,
      reason: text(data.reason),
    };
    if (!next.startedAt && ['running', 'waiting_human'].includes(status)) next.startedAt = event.at;
    if (!next.finishedAt && terminal(next.status)) next.finishedAt = event.at;
    if (status === 'failed')
      next.error = {
        severity: 'error',
        code: 'HISTORICAL_FAILURE',
        phase: 'runtime',
        message: event.message,
        path: '',
      };
    instances.set(next.id, next);
  }
  historical.instances = [...instances.values()];
  return historical;
}

export function stableValue(value: unknown): string {
  return (
    JSON.stringify(value, (_, item) =>
      item && typeof item === 'object' && !Array.isArray(item)
        ? Object.fromEntries(
            Object.keys(item)
              .sort()
              .map((key) => [key, item[key]]),
          )
        : item,
    ) ?? ''
  );
}

export interface ValueChange {
  key: string;
  before?: unknown;
  after?: unknown;
}
export function valueChanges(
  before: Record<string, unknown>,
  after: Record<string, unknown>,
): ValueChange[] {
  return [...new Set([...Object.keys(before), ...Object.keys(after)])]
    .sort()
    .flatMap((key) =>
      stableValue(before[key]) === stableValue(after[key])
        ? []
        : [{ key, before: before[key], after: after[key] }],
    );
}

function prompts(run: EngineRun): Record<string, unknown> {
  const result: Record<string, unknown> = {};
  const files = new Map(run.package.files.map((file) => [file.path, file.content]));
  function visit(scope: string, graph: Graph, path = '') {
    for (const [id, node] of Object.entries(graph.nodes)) {
      const address = `${path}/nodes/${id}`;
      const config = record(node[node.type]);
      if (['llm', 'agent', 'human'].includes(node.type)) {
        for (const source of ['prompt', 'instructions']) {
          if (!config[source]) continue;
          const prompt = record(config[source]);
          let contents: string | undefined;
          if (typeof prompt.file === 'string') {
            try {
              const file = files.get(prompt.file);
              if (file) contents = decodeText(file);
            } catch {
              /* Not a text resource. */
            }
          }
          result[`${scope}:${address}${source === 'prompt' ? '' : '/instructions'}`] =
            contents === undefined ? config[source] : { ...prompt, text: contents };
        }
      }
      if (['foreach', 'loop'].includes(node.type)) {
        const body = record(config.body);
        if (body.nodes) visit(scope, body as unknown as Graph, `${address}/body`);
      }
    }
  }
  for (const [scope, graph] of packageGraphs(run)) visit(scope, graph);
  return result;
}

export interface InstanceChange {
  key: string;
  before?: EngineInstance;
  after?: EngineInstance;
  beforeDuration?: number;
  afterDuration?: number;
  ambiguous: boolean;
  changed: boolean;
}

// Stable addresses include every parent iteration. Runtime IDs are unique per run
// and cannot be compared across runs; ambiguous legacy siblings stay unmatched.
export function instanceAddresses(
  run: EngineRun,
): Map<string, { key: string; ambiguous: boolean }> {
  const byId = new Map(run.instances.map((instance) => [instance.id, instance]));
  const result = new Map<string, { key: string; ambiguous: boolean }>();
  function address(
    instance: EngineInstance,
    visited = new Set<string>(),
  ): { key: string; ambiguous: boolean } {
    const cached = result.get(instance.id);
    if (cached) return cached;
    if (visited.has(instance.id)) return { key: instance.nodeId, ambiguous: true };
    const next = new Set(visited).add(instance.id);
    const parent = instance.parentInstanceId && byId.get(instance.parentInstanceId);
    const ancestor = parent ? address(parent, next) : undefined;
    const own = `${instance.scope}:${instance.graphPath || `/nodes/${instance.nodeId}`}${instance.iterationIndex === undefined ? '' : `[${instance.iterationIndex}]`}`;
    const value = {
      key: ancestor ? `${ancestor.key} → ${own}` : own,
      ambiguous: Boolean(ancestor?.ambiguous || (instance.parentInstanceId && !parent)),
    };
    result.set(instance.id, value);
    return value;
  }
  for (const instance of run.instances) address(instance);
  const counts = new Map<string, number>();
  for (const value of result.values()) counts.set(value.key, (counts.get(value.key) ?? 0) + 1);
  for (const value of result.values()) if (counts.get(value.key)! > 1) value.ambiguous = true;
  function ambiguousAncestor(instance: EngineInstance, visited = new Set<string>()): boolean {
    if (visited.has(instance.id) || result.get(instance.id)?.ambiguous) return true;
    if (!instance.parentInstanceId) return false;
    const parent = byId.get(instance.parentInstanceId);
    return !parent || ambiguousAncestor(parent, new Set(visited).add(instance.id));
  }
  for (const instance of run.instances)
    if (ambiguousAncestor(instance)) result.get(instance.id)!.ambiguous = true;
  return result;
}

function compareInstances(before: EngineRun, after: EngineRun): InstanceChange[] {
  const beforeAddresses = instanceAddresses(before),
    afterAddresses = instanceAddresses(after);
  const rows = new Map<string, InstanceChange>();
  for (const [run, side, addresses] of [
    [before, 'before', beforeAddresses],
    [after, 'after', afterAddresses],
  ] as const) {
    for (const instance of run.instances) {
      const address = addresses.get(instance.id)!;
      const key = address.ambiguous ? `${address.key} · ${run.id}/${instance.id}` : address.key;
      const row = rows.get(key) ?? {
        key: address.key,
        ambiguous: address.ambiguous,
        changed: false,
      };
      row[side] = instance;
      rows.set(key, row);
    }
  }
  return [...rows.values()]
    .map((row) => {
      row.beforeDuration = duration(row.before?.startedAt, row.before?.finishedAt);
      row.afterDuration = duration(row.after?.startedAt, row.after?.finishedAt);
      row.changed =
        !row.before ||
        !row.after ||
        row.before.status !== row.after.status ||
        row.beforeDuration !== row.afterDuration ||
        stableValue(row.before.inputs) !== stableValue(row.after.inputs) ||
        stableValue(row.before.outputs) !== stableValue(row.after.outputs);
      return row;
    })
    .sort((a, b) => a.key.localeCompare(b.key));
}

export function compareRuns(before: EngineRun, after: EngineRun) {
  const files = (run: EngineRun) =>
    Object.fromEntries(run.package.files.map((file) => [file.path, file.content]));
  return {
    source: valueChanges(
      { [before.package.entrypoint]: before.package.source },
      { [after.package.entrypoint]: after.package.source },
    ),
    files: valueChanges(files(before), files(after)),
    prompts: valueChanges(prompts(before), prompts(after)),
    inputs: valueChanges(before.inputs, after.inputs),
    inputArtifacts: valueChanges(before.inputArtifacts, after.inputArtifacts),
    outputs: valueChanges(before.outputs, after.outputs),
    instances: compareInstances(before, after),
  };
}
