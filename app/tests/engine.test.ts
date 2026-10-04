import { assertEngineModel } from '../src/lib/engine/validation';
import { freshState, readBackup } from '../src/lib/storage';
import { describe, expect, it, vi } from 'vitest';
import { enginePackage } from '../src/lib/engine/package';
import {
  EventParser,
  parseEngineJSON,
  connectEngine,
  disconnectEngine,
  engineCall,
  engineCache,
  eventWindow,
} from '../src/lib/engine/client';

describe('durable event framing', () => {
  it('handles arbitrary boundaries, CRLF, multiline data and heartbeat comments', () => {
    const parser = new EventParser();
    const frames = [];
    for (const char of ': heartbeat\r\nid: e1\r\ndata: {"message":"Привет",\r\ndata: "n":1}\r\n\r\n')
      frames.push(...parser.push(char));
    expect(frames).toEqual([{ id: 'e1', data: '{"message":"Привет",\n"n":1}' }]);
  });
  it('requires durable IDs and rejects oversized/injectable messages', () => {
    expect(() => new EventParser().push('data: {}\n\n')).toThrow('durable ID');
    expect(() => new EventParser().push('id: bad\0id\ndata: {}\n\n')).toThrow('Invalid event ID');
    expect(() => new EventParser().push('x'.repeat(256 * 1024 + 1))).toThrow('256 KiB');
    expect(() => new EventParser().push('я'.repeat(128 * 1024 + 1))).toThrow('256 KiB');
  });
});

it('bounds the live event cache by payload size while retaining newest events', () => {
  const events = Array.from({ length: 100 }, (_, n) => ({
    id: String(n),
    runId: 'r',
    at: '2026-10-04T00:00:00Z',
    type: 'model.completed',
    message: '',
    data: { content: 'x'.repeat(32 * 1024) },
  }));
  const window = eventWindow(events);
  expect(window.length).toBeLessThan(20);
  expect(window.at(-1)?.id).toBe('99');
  expect(window[0].id).toBe(String(100 - window.length));
  expect(eventWindow([])).toEqual([]);
});

it('loads scoped history without advancing the live cursor or caching full pages', async () => {
  const entries = new Map<string, string>();
  vi.stubGlobal('localStorage', {
    getItem: (key: string) => entries.get(key) ?? null,
    setItem: (key: string, value: string) => entries.set(key, value),
  });
  let wrongInstance = false;
  vi.stubGlobal(
    'fetch',
    vi.fn(async (target: URL) => {
      if (target.pathname === '/prefix/v1/info')
        return Response.json({
          protocol: 'knotra.desktop/1',
          engineId: 'history-test',
          principalId: 'local',
          version: '1',
          capabilities: ['history'],
        });
      expect(target.pathname).toBe('/prefix/v1/runs/run%2F1/history');
      expect(target.searchParams.get('cursor')).toBe('cursor/+');
      expect(target.searchParams.get('instanceId')).toBe('node/1');
      return Response.json({
        items: [
          {
            id: 'event1',
            runId: 'run/1',
            instanceId: wrongInstance ? 'other' : 'node/1',
            at: '2026-10-04T00:00:00Z',
            type: 'model.started',
            message: 'model.started',
          },
        ],
        nextCursor: null,
      });
    }),
  );
  try {
    await connectEngine('http://localhost:8080/prefix');
    const before = await engineCache();
    const call = {
      op: 'history' as const,
      runId: 'run/1',
      instanceId: 'node/1',
      cursor: 'cursor/+',
    };
    expect(await engineCall(call)).toMatchObject({ items: [{ id: 'event1' }] });
    expect(await engineCache()).toEqual(before);
    wrongInstance = true;
    await expect(engineCall(call)).rejects.toMatchObject({ code: 'protocol' });
  } finally {
    await disconnectEngine();
    vi.unstubAllGlobals();
  }
});

it('rejects unsafe integers consistently in REST and SSE JSON before accepting data', () => {
  expect(parseEngineJSON('{"n":9007199254740991,"fraction":1.25}')).toEqual({
    n: Number.MAX_SAFE_INTEGER,
    fraction: 1.25,
  });
  for (const text of ['{"data":{"values":[9007199254740993]}}', '{"n":1e20}', '{"n":-1e20}']) {
    expect(() => parseEngineJSON(text)).toThrow('safe range');
  }
  expect(() => parseEngineJSON('{')).toThrow('invalid JSON');
});

it('does not overwrite the last valid cached page with a malformed read model', async () => {
  const entries = new Map<string, string>();
  vi.stubGlobal('localStorage', {
    getItem: (key: string) => entries.get(key) ?? null,
    setItem: (key: string, value: string) => entries.set(key, value),
  });
  const good = {
    items: [
      {
        id: 'r1',
        definitionId: 'd1',
        title: 'Saved run',
        status: 'running',
        createdAt: '2026-10-04T00:00:00Z',
        updatedAt: '2026-10-04T00:00:00Z',
        profile: 'local',
        package: enginePackage(freshState().workspaces[0]),
        inputs: {},
        inputArtifacts: {},
        outputs: {},
        artifacts: [],
        instances: [],
        diagnostics: [],
        availableActions: ['cancel'],
      },
    ],
    nextCursor: null,
  };
  const pages = [good, { items: [{ id: 'r1', status: 'failed' }], nextCursor: null }];
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async (target: URL) =>
        new Response(
          JSON.stringify(
            target.pathname === '/v1/info'
              ? {
                  protocol: 'knotra.desktop/1',
                  engineId: 'cache-test',
                  principalId: 'local',
                  version: '1',
                  capabilities: [],
                }
              : pages.shift(),
          ),
          { status: 200, headers: { 'Content-Type': 'application/json' } },
        ),
    ),
  );
  try {
    await connectEngine('http://localhost:8080');
    const call = { op: 'runs' as const, cursor: null };
    expect(await engineCall(call)).toEqual(good);
    const before = await engineCache();
    await expect(engineCall(call)).rejects.toMatchObject({ code: 'protocol' });
    expect(await engineCache()).toEqual(before);
  } finally {
    await disconnectEngine();
    vi.unstubAllGlobals();
  }
});

it('does not expose an old-session receipt after reconnect during cache reconciliation', async () => {
  vi.resetModules();
  let release!: (value: { cache: {}; pending: [] }) => void;
  const heldCache = new Promise<{ cache: {}; pending: [] }>((resolve) => {
    release = resolve;
  });
  const empty = { cache: {}, pending: [] as [] };
  let cacheCalls = 0;
  let identity = 0;
  const getCache = vi.fn(() => (++cacheCalls === 2 ? heldCache : Promise.resolve(empty)));
  vi.doMock('react', () => ({
    useState: (value: unknown) => [value, () => {}],
    useRef: (value: unknown) => ({ current: value }),
    useCallback: (callback: unknown) => callback,
    useEffect: () => {},
  }));
  vi.doMock('../src/lib/engine/client', async () => ({
    ...(await vi.importActual('../src/lib/engine/client')),
    connectEngine: vi.fn(async () => ({
      protocol: 'knotra.desktop/1',
      engineId: 'test',
      principalId: `identity-${++identity}`,
      version: '1',
      capabilities: [],
    })),
    disconnectEngine: vi.fn(async () => {}),
    engineCall: vi.fn(async () => ({ accepted: true, runId: 'old-run' })),
    engineCache: getCache,
  }));
  try {
    const { useEngine } = await import('../src/lib/engine/useEngine');
    const engine = useEngine(vi.fn());
    await engine.connect('http://localhost:8080');
    const receipt = engine.command({
      op: 'cancel',
      runId: 'old-run',
      operationId: 'saved-command',
    });
    await vi.waitFor(() => expect(getCache).toHaveBeenCalledTimes(2));
    await engine.connect('http://localhost:9090');
    release(empty);
    await expect(receipt).rejects.toMatchObject({ code: 'disconnected' });
  } finally {
    vi.doUnmock('react');
    vi.doUnmock('../src/lib/engine/client');
    vi.resetModules();
  }
});

it('checks engine models against the documented contract rather than trusting JSON', () => {
  expect(() =>
    assertEngineModel('Info', {
      protocol: 'knotra.desktop/1',
      engineId: 'fixture',
      principalId: 'test-user',
      version: '1',
      capabilities: [],
    }),
  ).not.toThrow();
  expect(() => assertEngineModel('Run', { id: 'partial', status: 'succeeded' })).toThrow(
    'invalid Run',
  );
  expect(() =>
    assertEngineModel('Info', {
      protocol: 'other',
      engineId: 'fixture',
      principalId: 'test-user',
      version: '1',
      capabilities: [],
    }),
  ).toThrow('invalid Info');
});

it('restores complete authoring bytes and rejects ambiguous or hostile backups', () => {
  const original = freshState();
  original.theme = 'light';
  expect(readBackup(JSON.stringify(original))).toEqual(original);
  original.workspaces.push(original.workspaces[0]);
  expect(() => readBackup(JSON.stringify(original))).toThrow('Invalid pipeline');
  original.workspaces.pop();
  original.workspaces[0].files.push({ path: '../escape', content: 'AA==' });
  expect(() => readBackup(JSON.stringify(original))).toThrow('package path');
});

it('uses the engine ASCII path folding rules when restoring supporting files', () => {
  const original = freshState();
  original.workspaces[0].files = [
    { path: 'Ä.txt', content: 'AA==' },
    { path: 'ä.txt', content: 'AA==' },
  ];
  expect(readBackup(JSON.stringify(original))).toEqual(original);
  original.workspaces[0].files.push({ path: 'DATA.txt', content: 'AA==' });
  original.workspaces[0].files.push({ path: 'data.txt', content: 'AA==' });
  expect(() => readBackup(JSON.stringify(original))).toThrow('package path');
});
