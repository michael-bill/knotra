import { decodeText } from '../bytes';
import { parsePipeline } from '../validation';
import { record, type Graph, type NodeDefinition } from '../types';
import type { EngineArtifact, EngineEvent, EngineInstance, EngineRun, EngineStatus } from './types';

export interface ExecutionGraph {
  key: string;
  scope: string;
  path: string;
  parentId?: string;
  iterationIndex?: number;
  graph: Graph;
  instances: EngineInstance[];
}

export function packageGraphs(run: EngineRun): Map<string, Graph> {
  const graphs = new Map<string, Graph>();
  const root = parsePipeline(run.package.source);
  if (root) graphs.set(run.package.entrypoint, root.spec);
  const files = new Map(run.package.files.map((file) => [file.path, file]));
  const imports = (graph: Graph): string[] =>
    Object.values(graph.nodes).flatMap((node) => {
      if (node.type === 'pipeline') return [String(record(node.pipeline).file)];
      if (node.type === 'foreach' || node.type === 'loop') {
        const body = record(record(node[node.type]).body);
        return body.nodes ? imports(body as unknown as Graph) : [];
      }
      return [];
    });
  const pending = root ? imports(root.spec) : [];
  const visited = new Set<string>();
  while (pending.length) {
    const path = pending.pop()!;
    if (visited.has(path)) continue;
    visited.add(path);
    const file = files.get(path);
    if (!file || graphs.has(path)) continue;
    try {
      const child = parsePipeline(decodeText(file.content));
      if (child) {
        graphs.set(file.path, child.spec);
        pending.push(...imports(child.spec));
      }
    } catch {
      // A non-text package artifact is not a graph.
    }
  }
  return graphs;
}

function graphAt(root: Graph, path: string): Graph | undefined {
  let graph = root;
  const segments = path.split('/').filter(Boolean);
  for (let i = 0; i < segments.length; i += 3) {
    if (segments[i] !== 'nodes' || segments[i + 2] !== 'body') return;
    const node = graph.nodes[segments[i + 1]];
    if (!node || !['foreach', 'loop'].includes(node.type)) return;
    const body = record(record(node[node.type]).body);
    if (!body.nodes) return;
    graph = body as unknown as Graph;
  }
  return graph;
}

export function executionGraphs(run: EngineRun, graphs: Map<string, Graph>): ExecutionGraph[] {
  const root = graphs.get(run.package.entrypoint);
  const result: ExecutionGraph[] = root
    ? [{ key: 'root', scope: run.package.entrypoint, path: '', graph: root, instances: [] }]
    : [];
  for (const instance of run.instances) {
    const scope = instance.scope || run.package.entrypoint;
    const path = (instance.graphPath ?? '').replace(/\/nodes\/[^/]+$/, '');
    const key =
      !instance.parentInstanceId && !path && scope === run.package.entrypoint
        ? 'root'
        : `${scope}:${path}:${instance.parentInstanceId ?? ''}:${instance.iterationIndex ?? ''}`;
    let group = result.find((entry) => entry.key === key);
    if (!group) {
      const file = graphs.get(scope);
      const graph = file && graphAt(file, path);
      if (!graph) continue;
      group = {
        key,
        scope,
        path,
        parentId: instance.parentInstanceId,
        iterationIndex: instance.iterationIndex,
        graph,
        instances: [],
      };
      result.push(group);
    }
    group.instances.push(instance);
  }
  return result;
}

// A group's nodes are distinct runtime instances. Never collapse repeated node IDs across scopes.
export function graphInstances(group?: ExecutionGraph): Record<string, EngineInstance> {
  return Object.fromEntries(
    (group?.instances ?? []).map((instance) => [instance.nodeId, instance]),
  );
}

export function mergeEvents(...batches: EngineEvent[][]): EngineEvent[] {
  const events = new Map<string, EngineEvent>();
  for (const batch of batches) for (const event of batch) events.set(event.id, event);
  return [...events.values()].sort((a, b) => {
    // Durable event IDs are monotonic decimal offsets; timestamps may coincide within a batch.
    if (/^\d+$/.test(a.id) && /^\d+$/.test(b.id))
      return a.id.length - b.id.length || a.id.localeCompare(b.id);
    return a.at.localeCompare(b.at) || a.id.localeCompare(b.id);
  });
}

export function observationWindow(events: EngineEvent[]): EngineEvent[] {
  let size = 0;
  let start = events.length;
  while (start > 0 && events.length - start < 5000) {
    const next = JSON.stringify(events[start - 1]).length;
    if (size + next > 4 * 1024 * 1024) break;
    size += next;
    start--;
  }
  return events.slice(start);
}

export interface Activity {
  id: string;
  kind: 'model' | 'tool' | 'event';
  at: string;
  attemptId?: string;
  step?: number;
  started?: EngineEvent;
  completed?: EngineEvent;
  events: EngineEvent[];
  text: string;
  truncated?: boolean;
  observationIncomplete?: boolean;
}

export function activities(events: EngineEvent[]): Activity[] {
  const rows: Activity[] = [];
  const calls = new Map<string, Activity>();
  for (const event of events) {
    const data = record(event.data);
    const kind = event.type.startsWith('model.')
      ? 'model'
      : event.type.startsWith('tool.')
        ? 'tool'
        : 'event';
    if (kind === 'event') {
      rows.push({
        id: event.id,
        kind,
        at: event.at,
        events: [event],
        text: '',
        attemptId: event.attemptId,
      });
      continue;
    }
    const step = typeof data.step === 'number' ? data.step : undefined;
    const key = `${event.attemptId ?? ''}:${kind}:${data.callId ?? event.operationId ?? data.operationId ?? `${step ?? ''}:${data.name ?? ''}`}`;
    let row = calls.get(key);
    // Legacy events without operation IDs can still carry repeated calls in one attempt.
    if (!row || (event.type.endsWith('.started') && row.completed)) {
      row = {
        id: event.id,
        kind,
        at: event.at,
        step,
        attemptId: event.attemptId,
        events: [],
        text: '',
      };
      calls.set(key, row);
      rows.push(row);
    }
    if (!event.type.endsWith('.delta')) row.events.push(event);
    row.truncated ||= data.truncated === true;
    row.observationIncomplete ||= data.observationIncomplete === true;
    if (event.type.endsWith('.started')) row.started = event;
    if (event.type.endsWith('.delta')) {
      const text = row.text + String(data.text ?? data.content ?? '');
      row.truncated ||= text.length > 128 * 1024;
      row.text = text.slice(-128 * 1024);
    }
    if (event.type.endsWith('.completed') || event.type.endsWith('.failed')) {
      row.completed = event;
      const content =
        typeof data.text === 'string'
          ? data.text
          : typeof data.content === 'string'
            ? data.content
            : undefined;
      // Completion previews may be shorter than the already received stream. Keep that
      // stream when the engine explicitly says the completion preview was truncated.
      if (content !== undefined && !(data.truncated === true && row.text.length > content.length)) {
        row.truncated ||= content.length > 128 * 1024;
        row.text = content.slice(0, 128 * 1024);
      }
    }
  }
  return rows;
}

export function duration(start?: string, end?: string | number): number | undefined {
  if (!start || !end) return;
  const value = (typeof end === 'number' ? end : Date.parse(end)) - Date.parse(start);
  return Number.isFinite(value) ? Math.max(0, value) : undefined;
}

export function durationLabel(milliseconds: number | undefined): string {
  if (milliseconds === undefined) return '—';
  if (milliseconds < 1000) return `${Math.round(milliseconds)} ms`;
  if (milliseconds < 60_000) return `${(milliseconds / 1000).toFixed(1)} s`;
  return `${Math.floor(milliseconds / 60_000)} m ${Math.floor((milliseconds % 60_000) / 1000)} s`;
}

export function graphStatuses(group: ExecutionGraph): Record<string, EngineStatus> {
  const instances = graphInstances(group);
  return Object.fromEntries(
    Object.keys(group.graph.nodes).map((id) => [id, instances[id]?.status ?? 'pending']),
  );
}

export function instanceArtifacts(run: EngineRun, instance?: EngineInstance): EngineArtifact[] {
  if (!instance) return [];
  const artifacts = new Map<string, EngineArtifact>();
  for (const [port, value] of Object.entries(instance.outputs?.artifacts ?? {})) {
    for (const [index, entry] of (Array.isArray(value) ? value : [value]).entries()) {
      const ref = record(entry);
      if (typeof ref.id !== 'string') continue;
      artifacts.set(ref.id, {
        id: ref.id,
        name:
          typeof ref.name === 'string'
            ? ref.name
            : Array.isArray(value)
              ? `${port} [${index + 1}]`
              : port,
        mediaType: typeof ref.mediaType === 'string' ? ref.mediaType : 'application/octet-stream',
        size: typeof ref.size === 'number' ? ref.size : 0,
        sha256: typeof ref.sha256 === 'string' ? ref.sha256 : '',
        origin: { runId: run.id, instanceId: instance.id },
      });
    }
  }
  // Published artifacts contain the authoritative display metadata when available.
  for (const artifact of run.artifacts)
    if (artifact.origin.instanceId === instance.id || artifacts.has(artifact.id))
      artifacts.set(artifact.id, artifact);
  return [...artifacts.values()];
}

export function nodeForInstance(
  instance: EngineInstance,
  groups: ExecutionGraph[],
): NodeDefinition | undefined {
  return groups.find((group) => group.instances.some((item) => item.id === instance.id))?.graph
    .nodes[instance.nodeId];
}
