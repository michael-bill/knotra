import { describe, expect, it } from 'vitest';
import {
  appendReplayEvents,
  compareRuns,
  instanceAddresses,
  replayRun,
} from '../src/lib/engine/history';
import type { EngineEvent, EngineInstance, EngineRun } from '../src/lib/engine/types';
import { base64 } from '../src/lib/bytes';

const at = (seconds: number) => new Date(Date.UTC(2026, 9, 4, 12, 0, seconds)).toISOString();
function run(id = 'run'): EngineRun {
  return {
    id,
    definitionId: 'definition',
    title: 'Test',
    status: 'succeeded',
    createdAt: at(0),
    updatedAt: at(20),
    profile: 'local',
    package: {
      entrypoint: 'pipeline.yaml',
      source:
        'apiVersion: knotra/v1\nkind: Pipeline\nmetadata: {name: test}\nspec:\n  nodes:\n    writer:\n      type: llm\n      llm:\n        model: local\n        prompt: {file: prompts/writer.txt}\n      outputs:\n        answer: {schema: {type: string}}\n  outputs:\n    answer: {schema: {type: string}, bind: {from: nodes.writer.outputs.answer}}\n',
      files: [
        { path: 'prompts/writer.txt', content: base64(new TextEncoder().encode('Old prompt')) },
      ],
    },
    inputs: { question: 'hello' },
    inputArtifacts: {},
    outputs: { answer: 'FUTURE RESULT' },
    artifacts: [],
    diagnostics: [],
    availableActions: [],
    instances: [
      {
        id: 'node',
        nodeId: 'writer',
        scope: 'pipeline.yaml',
        graphPath: '/nodes/writer',
        status: 'succeeded',
        startedAt: at(1),
        finishedAt: at(20),
        outputs: { values: { answer: 'FUTURE RESULT' }, artifacts: {} },
      },
    ],
  };
}
function event(id: string, status: string, extra: Partial<EngineEvent> = {}): EngineEvent {
  return {
    id,
    runId: 'run',
    at: extra.at ?? at(Number(id)),
    type: 'node',
    message: status,
    instanceId: 'node',
    data: { status },
    ...extra,
  };
}

describe('historical state reconstruction', () => {
  it('never exposes future status, I/O, timing, diagnostics or commands', () => {
    const latest = run();
    latest.availableActions = ['cancel'];
    latest.diagnostics = [
      { severity: 'error', code: 'future', phase: 'runtime', message: 'future', path: '' },
    ];
    const events = [
      event('1', 'running', {
        data: { status: 'running', inputs: { values: { question: 'hello' }, artifacts: {} } },
      }),
      event('2', 'succeeded', {
        data: { status: 'succeeded', outputs: { values: { answer: 'recorded' }, artifacts: {} } },
      }),
    ];
    const initial = replayRun(latest, events, 0);
    expect(initial).toMatchObject({
      status: 'pending',
      outputs: {},
      instances: [],
      availableActions: [],
      diagnostics: [],
    });
    const first = replayRun(latest, events, 1);
    expect(first.instances[0]).toMatchObject({
      nodeId: 'writer',
      status: 'running',
      startedAt: at(1),
      inputs: { values: { question: 'hello' } },
    });
    expect(first.instances[0].finishedAt).toBeUndefined();
    expect(first.instances[0].outputs).toBeUndefined();
    expect(replayRun(latest, events, 2).instances[0].outputs).toEqual({
      values: { answer: 'recorded' },
      artifacts: {},
    });
    expect(latest.instances[0].outputs?.values.answer).toBe('FUTURE RESULT');
  });
  it('reconstructs nested instances and root output only from saved events', () => {
    const events = [
      event('1', 'running', { type: 'run', instanceId: undefined }),
      event('2', 'succeeded', {
        instanceId: 'child',
        data: {
          status: 'succeeded',
          nodeId: 'work',
          scope: 'child.yaml',
          parentInstanceId: 'container',
          iterationIndex: 0,
          graphPath: '/nodes/map/body/nodes/work',
          outputs: { values: { value: 7 }, artifacts: {} },
        },
      }),
      event('3', 'succeeded', {
        type: 'run',
        instanceId: undefined,
        data: { status: 'succeeded', outputs: { values: { answer: 7 }, artifacts: {} } },
      }),
    ];
    const value = replayRun(run(), events, 2);
    expect(value.status).toBe('running');
    expect(value.outputs).toEqual({});
    expect(value.instances[0]).toMatchObject({
      id: 'child',
      scope: 'child.yaml',
      parentInstanceId: 'container',
      iterationIndex: 0,
      graphPath: '/nodes/map/body/nodes/work',
    });
    expect(replayRun(run(), events, 3).outputs).toEqual({ answer: 7 });
  });
  it('keeps the beginning of history, ignores model deltas and orders large cursor IDs without numeric coercion', () => {
    const first = event('9007199254740993', 'running', { at: at(1) }),
      second = event('9007199254740994', 'succeeded', { at: at(0) });
    expect(
      appendReplayEvents(
        [],
        [second, first, event('3', 'delta', { type: 'model.delta' })],
      ).events.map((item) => item.id),
    ).toEqual([first.id, second.id]);
    const bounded = appendReplayEvents([first], [second], 10000, 1);
    expect(bounded.limited).toBe(true);
    expect(bounded.events).toEqual([first]);
    expect(appendReplayEvents([first], [first]).events).toHaveLength(1);
  });
});

describe('run comparison', () => {
  it('matches nested iterations by lineage across different runtime IDs', () => {
    const before = run('before'),
      after = run('after');
    function nodes(prefix: string): EngineInstance[] {
      return [
        {
          id: prefix + 'map',
          nodeId: 'map',
          scope: 'pipeline.yaml',
          graphPath: '/nodes/map',
          status: 'succeeded',
        },
        ...[0, 1].flatMap((index) => [
          {
            id: `${prefix}sub${index}`,
            nodeId: 'sub',
            scope: 'pipeline.yaml',
            graphPath: '/nodes/map/body/nodes/sub',
            parentInstanceId: prefix + 'map',
            iterationIndex: index,
            status: 'succeeded' as const,
          },
          {
            id: `${prefix}work${index}`,
            nodeId: 'work',
            scope: 'child.yaml',
            graphPath: '/nodes/work',
            parentInstanceId: `${prefix}sub${index}`,
            status: 'succeeded' as const,
            startedAt: at(1),
            finishedAt: at(4),
          },
        ]),
      ];
    }
    before.instances = nodes('old');
    after.instances = nodes('new');
    after.instances[4].status = 'failed';
    const rows = compareRuns(before, after).instances;
    expect(rows).toHaveLength(5);
    expect(rows.every((row) => row.before && row.after)).toBe(true);
    expect(rows.filter((row) => row.changed)).toHaveLength(1);
    expect(rows.find((row) => row.after?.id === 'newwork1')).toMatchObject({
      before: { id: 'oldwork1' },
      beforeDuration: 3000,
      afterDuration: 3000,
    });
  });
  it('does not pair ambiguous legacy repeated node IDs or recurse forever on corrupt parents', () => {
    const before = run('before'),
      after = run('after');
    before.instances = [
      { id: 'a', nodeId: 'same', scope: 'pipeline.yaml', status: 'succeeded' },
      { id: 'b', nodeId: 'same', scope: 'pipeline.yaml', status: 'succeeded' },
    ];
    after.instances = [{ id: 'c', nodeId: 'same', scope: 'pipeline.yaml', status: 'succeeded' }];
    const rows = compareRuns(before, after).instances;
    expect(rows).toHaveLength(3);
    expect(rows.every((row) => !(row.before && row.after))).toBe(true);
    before.instances[0].parentInstanceId = 'b';
    before.instances[1].parentInstanceId = 'a';
    expect([...instanceAddresses(before).values()].every((item) => item.ambiguous)).toBe(true);
  });
  it('propagates ambiguous parent identities to descendants even when child names differ', () => {
    const before = run('before'),
      after = run('after');
    before.instances = [
      { id: 'parent1', nodeId: 'map', scope: 'pipeline.yaml', status: 'succeeded' },
      { id: 'parent2', nodeId: 'map', scope: 'pipeline.yaml', status: 'succeeded' },
      {
        id: 'child1',
        nodeId: 'work',
        scope: 'pipeline.yaml',
        parentInstanceId: 'parent1',
        status: 'succeeded',
      },
      {
        id: 'child2',
        nodeId: 'other',
        scope: 'pipeline.yaml',
        parentInstanceId: 'parent2',
        status: 'succeeded',
      },
    ];
    after.instances = [
      { id: 'new-parent', nodeId: 'map', scope: 'pipeline.yaml', status: 'succeeded' },
      {
        id: 'new-child',
        nodeId: 'work',
        scope: 'pipeline.yaml',
        parentInstanceId: 'new-parent',
        status: 'succeeded',
      },
    ];
    expect(instanceAddresses(before).get('child1')?.ambiguous).toBe(true);
    expect(
      compareRuns(before, after).instances.filter(
        (row) => row.before?.nodeId === 'work' || row.after?.nodeId === 'work',
      ),
    ).toHaveLength(2);
  });

  it('compares prompt file contents, immutable packages and JSON values regardless of key order', () => {
    const before = run('before'),
      after = run('after');
    before.inputs = { data: { a: 1, b: 2 } };
    after.inputs = { data: { b: 2, a: 1 } };
    after.package.files[0].content = base64(new TextEncoder().encode('New prompt'));
    after.outputs = { answer: 'changed' };
    const changes = compareRuns(before, after);
    expect(changes.inputs).toEqual([]);
    expect(changes.source).toEqual([]);
    expect(changes.prompts).toEqual([
      {
        key: 'pipeline.yaml:/nodes/writer',
        before: { file: 'prompts/writer.txt', text: 'Old prompt' },
        after: { file: 'prompts/writer.txt', text: 'New prompt' },
      },
    ]);
    expect(changes.files).toHaveLength(1);
    expect(changes.outputs).toHaveLength(1);
  });
});
