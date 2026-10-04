import { expect, test } from '@playwright/test';

const modulePath = '/src/lib/engine/browserEvents.ts';
const session = 'knotra.engine.v1:browser-event-migration';

test('a blocked open closes its late connection and permits a clean retry', async ({ page }) => {
  await page.goto('/');
  const result = await page.evaluate(
    async ({ modulePath, session }) => {
      const store = await import(modulePath);
      const open = IDBFactory.prototype.open;
      const close = IDBDatabase.prototype.close;
      let didClose!: () => void;
      const closed = new Promise<void>((resolve) => (didClose = resolve));
      IDBFactory.prototype.open = function (...args) {
        const request = open.apply(this, args);
        // Simulate a blocked notification before the underlying open finishes.
        queueMicrotask(() => request.dispatchEvent(new Event('blocked')));
        return request;
      };
      IDBDatabase.prototype.close = function () {
        close.call(this);
        didClose();
      };
      let failure = '';
      try {
        await store.browserEventState(session, 'run');
      } catch (error) {
        failure = String(error);
      } finally {
        IDBFactory.prototype.open = open;
      }
      await closed;
      IDBDatabase.prototype.close = close;
      return { failure, state: await store.browserEventState(session, 'run') };
    },
    { modulePath, session },
  );
  expect(result.failure).toContain('Close other tabs');
  expect(result.state.events).toEqual([]);
});

test('migrates legacy event IDs atomically and keeps deduplication after a page restart', async ({
  page,
}) => {
  await page.goto('/');
  await page.evaluate(
    ({ session }) => {
      const events = [
        {
          id: 'opaque/latest+',
          runId: 'run',
          at: '2026-10-04T00:00:00Z',
          type: 'node',
          message: 'latest',
        },
      ];
      localStorage.setItem(
        session,
        JSON.stringify({
          cache: {
            'eventIds:run': ['opaque/evicted+', 'opaque/latest+'],
            'events:run': events,
            'cursor:run': 'opaque/latest+',
            profiles: { saved: true },
          },
          pending: [{ id: 'keep-command' }],
        }),
      );
    },
    { session },
  );
  const migrated = await page.evaluate(
    async ({ modulePath, session }) => {
      const store = await import(modulePath);
      const state = await store.browserEventState(session, 'run');
      return { state, cache: JSON.parse(localStorage.getItem(session)!) };
    },
    { modulePath, session },
  );
  expect(migrated.state.cursor).toBe('opaque/latest+');
  expect(migrated.state.events).toHaveLength(1);
  expect(migrated.cache).toEqual({
    cache: { profiles: { saved: true } },
    pending: [{ id: 'keep-command' }],
  });
  await page.reload();
  const restored = await page.evaluate(
    async ({ modulePath, session }) => {
      const store = await import(modulePath);
      const duplicate = await store.acceptBrowserEvent(session, {
        id: 'opaque/evicted+',
        runId: 'run',
        at: '2026-10-04T00:00:00Z',
        type: 'node',
        message: 'old duplicate',
      });
      const state = await store.browserEventState(session, 'run');
      return { duplicate, state };
    },
    { modulePath, session },
  );
  expect(restored.duplicate).toBe(false);
  expect(restored.state.cursor).toBe('opaque/latest+');
  expect(restored.state.events).toHaveLength(1);
});

test('evicted events remain deduplicated without growing localStorage or ordering opaque IDs', async ({
  page,
}) => {
  test.setTimeout(60_000);
  await page.goto('/');
  const result = await page.evaluate(
    async ({ modulePath, session }) => {
      const store = await import(modulePath);
      const makeEvent = (id: string, runId = 'run') => ({
        id,
        runId,
        at: '2026-10-04T00:00:00Z',
        type: 'model.delta',
        message: '',
        data: { text: 'chunk' },
      });
      localStorage.setItem(session, JSON.stringify({ cache: { keep: 'unchanged' }, pending: [] }));
      const original = localStorage.getItem(session);
      for (let index = 0; index < 1005; index++)
        await store.acceptBrowserEvent(session, makeEvent(`opaque/${index}+`));
      const window = await store.browserEventState(session, 'run');
      const duplicate = await store.acceptBrowserEvent(session, makeEvent('opaque/0+'));
      const accepted = [];
      for (const id of ['z', 'a', '10', '2'])
        accepted.push(await store.acceptBrowserEvent(session, makeEvent(id)));
      const concurrent = await Promise.all([
        store.acceptBrowserEvent(session, makeEvent('same')),
        store.acceptBrowserEvent(session, makeEvent('same')),
      ]);
      const otherSession = await store.acceptBrowserEvent(
        `${session}:other-principal`,
        makeEvent('opaque/0+'),
      );
      const otherRun = await store.acceptBrowserEvent(session, makeEvent('opaque/0+', 'other-run'));
      return {
        ids: window.events.map((event: { id: string }) => event.id),
        duplicate,
        accepted,
        concurrent,
        otherSession,
        otherRun,
        state: await store.browserEventState(session, 'run'),
        localStorageUnchanged: localStorage.getItem(session) === original,
      };
    },
    { modulePath, session },
  );
  expect(result.ids).toHaveLength(1000);
  expect(result.ids).not.toContain('opaque/0+');
  expect(result.duplicate).toBe(false);
  expect(result.accepted).toEqual([true, true, true, true]);
  expect(result.concurrent.sort()).toEqual([false, true]);
  expect(result.otherSession).toBe(true);
  expect(result.otherRun).toBe(true);
  expect(result.state.cursor).toBe('same');
  expect(result.localStorageUnchanged).toBe(true);
  await page.reload();
  const duplicateAfterReload = await page.evaluate(
    async ({ modulePath, session }) => {
      const store = await import(modulePath);
      return store.acceptBrowserEvent(session, {
        id: 'opaque/0+',
        runId: 'run',
        at: '2026-10-04T00:00:00Z',
        type: 'node',
        message: 'old',
      });
    },
    { modulePath, session },
  );
  expect(duplicateAfterReload).toBe(false);
});

test('a failed event transaction advances neither cursor nor deduplication identity', async ({
  page,
}) => {
  await page.goto('/');
  const result = await page.evaluate(
    async ({ modulePath, session }) => {
      const store = await import(modulePath);
      const event = (id: string) => ({
        id,
        runId: 'run',
        at: '2026-10-04T00:00:00Z',
        type: 'node',
        message: id,
      });
      await store.acceptBrowserEvent(session, event('accepted'));
      const put = IDBObjectStore.prototype.put;
      IDBObjectStore.prototype.put = function (value, key) {
        if (this.name === 'runs' && value.cursor === 'rejected')
          throw new DOMException('Injected storage failure', 'QuotaExceededError');
        return put.call(this, value, key);
      };
      let failure = '';
      try {
        await store.acceptBrowserEvent(session, event('rejected'));
      } catch (error) {
        failure = String(error);
      } finally {
        IDBObjectStore.prototype.put = put;
      }
      const unchanged = await store.browserEventState(session, 'run');
      const retry = await store.acceptBrowserEvent(session, event('rejected'));
      return { failure, unchanged, retry, final: await store.browserEventState(session, 'run') };
    },
    { modulePath, session },
  );
  expect(result.failure).toContain('Injected storage failure');
  expect(result.unchanged.cursor).toBe('accepted');
  expect(result.unchanged.events.map((event: { id: string }) => event.id)).toEqual(['accepted']);
  expect(result.retry).toBe(true);
  expect(result.final.cursor).toBe('rejected');
});

test('failed migration retains the legacy cursor and IDs until the transaction succeeds', async ({
  page,
}) => {
  await page.goto('/');
  const result = await page.evaluate(
    async ({ modulePath, session }) => {
      const store = await import(modulePath);
      const legacy = {
        cache: { 'eventIds:run': ['legacy'], 'cursor:run': 'legacy', 'events:run': [] },
        pending: [],
      };
      localStorage.setItem(session, JSON.stringify(legacy));
      const put = IDBObjectStore.prototype.put;
      IDBObjectStore.prototype.put = function (value, key) {
        if (this.name === 'runs')
          throw new DOMException('Migration interrupted', 'QuotaExceededError');
        return put.call(this, value, key);
      };
      let failed = false;
      try {
        await store.browserEventState(session, 'run');
      } catch {
        failed = true;
      } finally {
        IDBObjectStore.prototype.put = put;
      }
      const preserved = JSON.parse(localStorage.getItem(session)!);
      const state = await store.browserEventState(session, 'run');
      return {
        failed,
        preserved,
        legacy,
        state,
        cleaned: JSON.parse(localStorage.getItem(session)!),
      };
    },
    { modulePath, session },
  );
  expect(result.failed).toBe(true);
  expect(result.preserved).toEqual(result.legacy);
  expect(result.state.cursor).toBe('legacy');
  expect(result.cleaned).toEqual({ cache: {}, pending: [] });
});
