import { describe, expect, it } from 'vitest';
import {
  activities,
  duration,
  executionGraphs,
  graphInstances,
  instanceArtifacts,
  graphStatuses,
  mergeEvents,
  observationWindow,
} from '../src/lib/engine/observation';
import type { EngineEvent, EngineInstance, EngineRun } from '../src/lib/engine/types';
import type { Graph } from '../src/lib/types';

const leaf: Graph = { nodes: { summarize: { type: 'llm' } }, outputs: {} };
const root: Graph = {
  nodes: { batches: { type: 'foreach', foreach: { body: leaf } }, summarize: { type: 'llm' } },
  outputs: {},
};
function run(instances: EngineInstance[] = []): EngineRun {
  return {
    id: 'run',
    definitionId: 'definition',
    title: 'Run',
    status: 'running',
    createdAt: '2026-10-04T00:00:00Z',
    updatedAt: '2026-10-04T00:00:04Z',
    profile: 'local',
    package: { entrypoint: 'root.yaml', source: '', files: [] },
    inputs: {},
    inputArtifacts: {},
    outputs: {},
    artifacts: [],
    instances,
    diagnostics: [],
    availableActions: [],
  };
}
function event(
  id: string,
  type: string,
  data: EngineEvent['data'],
  operationId = 'call',
  attemptId = 'attempt',
): EngineEvent {
  return {
    id,
    type,
    data,
    operationId,
    attemptId,
    runId: 'run',
    instanceId: 'instance',
    at: '2026-10-04T00:00:00Z',
    message: type,
  };
}

describe('runtime graph identity', () => {
  it('keeps equal node names in root, loop iterations and imported pipeline distinct', () => {
    const instances: EngineInstance[] = [
      {
        id: 'root-summary',
        nodeId: 'summarize',
        scope: 'root.yaml',
        graphPath: '/nodes/summarize',
        status: 'succeeded',
      },
      {
        id: 'iteration-0',
        nodeId: 'summarize',
        scope: 'root.yaml',
        graphPath: '/nodes/batches/body/nodes/summarize',
        parentInstanceId: 'batches-instance',
        iterationIndex: 0,
        status: 'running',
      },
      {
        id: 'iteration-1',
        nodeId: 'summarize',
        scope: 'root.yaml',
        graphPath: '/nodes/batches/body/nodes/summarize',
        parentInstanceId: 'batches-instance',
        iterationIndex: 1,
        status: 'failed',
      },
      {
        id: 'child-summary',
        nodeId: 'summarize',
        scope: 'child.yaml',
        graphPath: '/nodes/summarize',
        parentInstanceId: 'child-instance',
        status: 'waiting_human',
      },
    ];
    const groups = executionGraphs(
      run(instances),
      new Map([
        ['root.yaml', root],
        ['child.yaml', leaf],
      ]),
    );
    expect(groups).toHaveLength(4);
    expect(groups.map((group) => graphInstances(group).summarize.id)).toEqual([
      'root-summary',
      'iteration-0',
      'iteration-1',
      'child-summary',
    ]);
    expect(groups.map((group) => graphStatuses(group).summarize)).toEqual([
      'succeeded',
      'running',
      'failed',
      'waiting_human',
    ]);
    expect(graphStatuses(groups[0]).batches).toBe('pending');
  });
  it('does not attach a nested instance to a root node when its graph path is unavailable', () => {
    const groups = executionGraphs(
      run([
        {
          id: 'nested',
          nodeId: 'summarize',
          scope: 'root.yaml',
          graphPath: '/nodes/missing/body/nodes/summarize',
          parentInstanceId: 'unknown',
          status: 'failed',
        },
      ]),
      new Map([['root.yaml', root]]),
    );
    expect(groups).toHaveLength(1);
    expect(graphInstances(groups[0])).toEqual({});
  });
});

describe('durable observation trajectories', () => {
  it('orders decimal durable IDs without precision loss and deduplicates cache, stream and history', () => {
    const earlier = event('9007199254740992', 'model.started', {});
    const later = event('9007199254740993', 'model.completed', {});
    expect(mergeEvents([later], [earlier, later]).map((item) => item.id)).toEqual([
      earlier.id,
      later.id,
    ]);
    expect(
      mergeEvents([event('10', 'x', {})], [event('9', 'x', {})]).map((item) => item.id),
    ).toEqual(['9', '10']);
  });
  it('reconstructs text, prefers completed content and separates attempts and operations', () => {
    const rows = activities([
      event('1', 'model.started', { step: 1, messages: [{ role: 'user', content: 'Prompt' }] }),
      event('2', 'model.delta', { step: 1, text: 'partial' }),
      event('3', 'tool.started', { step: 1, name: 'files.read', arguments: {} }, 'tool-call'),
      event(
        '4',
        'tool.completed',
        { step: 1, result: { isError: true, error: 'Not found' } },
        'tool-call',
      ),
      event('5', 'model.completed', {
        step: 1,
        content: 'Final content',
        inputTokens: 5,
        outputTokens: 3,
      }),
      event('6', 'model.started', { step: 1 }, 'call', 'retry-attempt'),
      event('7', 'model.delta', { step: 1, text: 'New ' }, 'call', 'retry-attempt'),
      event('8', 'model.delta', { step: 1, text: 'response' }, 'call', 'retry-attempt'),
      event('9', 'model.failed', { step: 1, error: 'Connection lost' }, 'call', 'retry-attempt'),
    ]);
    expect(rows).toHaveLength(3);
    expect(rows[0].text).toBe('Final content');
    expect(rows[0].started?.data).toHaveProperty('messages');
    expect(rows[1].completed?.data).toHaveProperty('result.isError', true);
    expect(rows[2].text).toBe('New response');
    expect(rows[2].completed?.type).toBe('model.failed');
    expect(rows[0].events).toHaveLength(2);
  });
  it('bounds memory even when providers send large data', () => {
    const values = Array.from({ length: 100 }, (_, i) =>
      event(String(i), 'model.started', { messages: [{ content: 'x'.repeat(64 * 1024) }] }),
    );
    const bounded = observationWindow(values);
    expect(bounded.length).toBeLessThan(values.length);
    expect(bounded.at(-1)?.id).toBe('99');
    expect(JSON.stringify(bounded).length).toBeLessThan(4 * 1024 * 1024 + 100);
    expect(
      observationWindow(
        Array.from({ length: 6000 }, (_, i) => event(String(i), 'model.delta', { text: 'x' })),
      ),
    ).toHaveLength(5000);
  });
  it('does not invent timing for old snapshots and clamps clock skew', () => {
    expect(duration(undefined, Date.now())).toBeUndefined();
    expect(duration('bad', 'also bad')).toBeUndefined();
    expect(duration('2026-10-04T00:00:04Z', '2026-10-04T00:00:03Z')).toBe(0);
  });
});

it('lists intermediate and collection artifacts and prefers published metadata without duplicates', () => {
  const value = run();
  const instance: EngineInstance = {
    id: 'producer',
    nodeId: 'summarize',
    scope: 'root.yaml',
    status: 'succeeded',
    outputs: {
      values: {},
      artifacts: {
        document: { id: 'intermediate', mediaType: 'text/plain', size: 12, sha256: 'hash' },
        collection: [{ id: 'published', mediaType: 'application/json', size: 15, sha256: 'hash2' }],
      },
    },
  };
  value.artifacts = [
    {
      id: 'published',
      name: 'result.json',
      mediaType: 'application/json',
      size: 15,
      sha256: 'hash2',
      origin: { instanceId: 'producer' },
    },
  ];
  expect(instanceArtifacts(value, instance).map((file) => [file.id, file.name])).toEqual([
    ['intermediate', 'document'],
    ['published', 'result.json'],
  ]);
  expect(instanceArtifacts(value)).toEqual([]);
});

it('keeps longer streamed text when a completion preview is truncated and retains delta warnings', () => {
  const text = 'x'.repeat(50 * 1024);
  const rows = activities([
    event('1', 'model.started', { step: 1 }),
    event('2', 'model.delta', { step: 1, text, observationIncomplete: true }),
    event('3', 'model.completed', { step: 1, content: text.slice(0, 16 * 1024), truncated: true }),
  ]);
  expect(rows[0].text).toBe(text);
  expect(rows[0].truncated).toBe(true);
  expect(rows[0].observationIncomplete).toBe(true);
  const interrupted = activities([event('1', 'model.delta', { text: 'partial', truncated: true })]);
  expect(interrupted[0].truncated).toBe(true);
});
