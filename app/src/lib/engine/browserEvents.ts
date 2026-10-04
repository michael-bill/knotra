import { EngineError } from './error';
import type { EngineCache, EngineEvent } from './types';

const DATABASE = 'knotra.engine.events.v1';
let database: Promise<IDBDatabase> | undefined;

interface EventState {
  session: string;
  runId: string;
  cursor?: string;
  events: EngineEvent[];
}

// IDs remain exact and opaque. IndexedDB keeps the growing deduplication set off
// the synchronous, quota-limited localStorage snapshot used by other UI state.
function openDatabase(): Promise<IDBDatabase> {
  if (database) return database;
  database = new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open(DATABASE, 1);
    let failed = false;
    const fail = (error: unknown) => {
      failed = true;
      reject(error);
    };
    request.onupgradeneeded = () => {
      request.result.createObjectStore('runs', { keyPath: ['session', 'runId'] });
      request.result.createObjectStore('ids');
    };
    request.onsuccess = () => {
      const db = request.result;
      // A blocked upgrade can finish after its caller has already received an
      // error. Do not leave that now-unowned connection blocking future opens.
      if (failed) {
        db.close();
        return;
      }
      db.onversionchange = () => {
        db.close();
        database = undefined;
      };
      resolve(db);
    };
    request.onerror = () => fail(request.error);
    request.onblocked = () => fail(new Error('Close other tabs to upgrade event storage.'));
  }).catch((error) => {
    database = undefined;
    throw storageError(error);
  });
  return database;
}

function storageError(error: unknown): EngineError {
  return new EngineError(
    'storage',
    `Engine event storage could not be updated. ${error instanceof Error ? error.message : ''}`.trim(),
  );
}

function transaction<T>(
  db: IDBDatabase,
  operation: (
    tx: IDBTransaction,
    result: (value: T) => void,
    abort: (error: unknown) => void,
  ) => void,
): Promise<T> {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(['runs', 'ids'], 'readwrite', { durability: 'strict' });
    let result: T;
    let failure: unknown;
    const abort = (error: unknown) => {
      failure = error;
      tx.abort();
    };
    tx.oncomplete = () => resolve(result);
    tx.onabort = () => reject(storageError(failure ?? tx.error));
    try {
      operation(tx, (value) => (result = value), abort);
    } catch (error) {
      abort(error);
    }
  });
}

// Detailed history stays on the engine. Bound the materialized browser window
// independently of the exact deduplication set and opaque reconnect cursor.
export function eventWindow(events: EngineEvent[]): EngineEvent[] {
  let characters = 0;
  let start = events.length;
  while (start > 0 && events.length - start < 1000) {
    const size = JSON.stringify(events[start - 1]).length;
    if (characters + size > 512 * 1024 && start < events.length) break;
    characters += size;
    start--;
  }
  return events.slice(start);
}

function legacyCache(session: string): EngineCache {
  return JSON.parse(localStorage.getItem(session) ?? '{"cache":{},"pending":[]}');
}

function removeLegacyEvents(session: string, runId: string) {
  // Re-read after the transaction: unrelated commands may update their cache
  // while IndexedDB is committing. Never write back a stale whole-cache copy.
  try {
    const value = legacyCache(session);
    const keys = [`events:${runId}`, `eventIds:${runId}`, `cursor:${runId}`];
    if (!keys.some((key) => Object.hasOwn(value.cache, key))) return;
    for (const key of keys) delete value.cache[key];
    localStorage.setItem(session, JSON.stringify(value));
  } catch {
    // The committed IndexedDB record is authoritative. A later read retries
    // cleanup; failing cleanup must not undo an accepted event or its cursor.
  }
}

async function withEventState<T>(
  session: string,
  runId: string,
  operation: (
    tx: IDBTransaction,
    state: EventState,
    result: (value: T) => void,
    abort: (error: unknown) => void,
  ) => void,
): Promise<T> {
  const db = await openDatabase();
  let migrated = false;
  const value = await transaction<T>(db, (tx, result, abort) => {
    const runs = tx.objectStore('runs');
    const existing = runs.get([session, runId]);
    existing.onsuccess = () => {
      try {
        if (existing.result) {
          operation(tx, existing.result as EventState, result, abort);
          return;
        }
        const saved = legacyCache(session).cache;
        const events = (saved[`events:${runId}`] ?? []) as EngineEvent[];
        const cursor = saved[`cursor:${runId}`] as string | undefined;
        const ids = new Set([
          ...((saved[`eventIds:${runId}`] ?? []) as string[]),
          ...events.map((event) => event.id),
          ...(cursor ? [cursor] : []),
        ]);
        for (const id of ids) tx.objectStore('ids').put(true, [session, runId, id]);
        const state = { session, runId, cursor, events: eventWindow(events) };
        runs.put(state);
        migrated = true;
        operation(tx, state, result, abort);
      } catch (error) {
        abort(error);
      }
    };
  });
  if (migrated) removeLegacyEvents(session, runId);
  return value;
}

export function browserEventState(session: string, runId: string): Promise<EventState> {
  return withEventState<EventState>(session, runId, (_, state, result) => result(state)).then(
    (state) => {
      removeLegacyEvents(session, runId);
      return state;
    },
  );
}

export function acceptBrowserEvent(session: string, event: EngineEvent): Promise<boolean> {
  return withEventState<boolean>(session, event.runId, (tx, state, result, abort) => {
    const ids = tx.objectStore('ids');
    const identity = [session, event.runId, event.id];
    const seen = ids.get(identity);
    seen.onsuccess = () => {
      try {
        if (seen.result) {
          result(false);
          return;
        }
        ids.put(true, identity);
        tx.objectStore('runs').put({
          ...state,
          cursor: event.id,
          events: eventWindow([...state.events, event]),
        });
        result(true);
      } catch (error) {
        abort(error);
      }
    };
  });
}
