import { Channel, invoke } from '@tauri-apps/api/core';
import { desktop } from '../native';
import { base64, sha256 } from '../bytes';
import {
  PROTOCOL,
  type EngineArtifact,
  type EngineCache,
  type EngineCall,
  type EngineInfo,
  type EngineEvent,
  type Page,
  type StreamMessage,
} from './types';
import { EngineError, engineError } from './error';

export { EngineError, engineError } from './error';

let connection: { url: string; token?: string; key: string } | undefined;
let sessionRevision = 0;
let browserWrites: Promise<void> = Promise.resolve();
const stable = (value: unknown): string =>
  JSON.stringify(value, (_, next) =>
    next && typeof next === 'object' && !Array.isArray(next)
      ? Object.fromEntries(
          Object.keys(next)
            .sort()
            .map((key) => [key, next[key]]),
        )
      : next,
  );
const watches = new Set<AbortController>();

function cache(): EngineCache {
  try {
    return JSON.parse(localStorage.getItem(connection!.key) ?? '{"cache":{},"pending":[]}');
  } catch {
    throw new EngineError('storage', 'Engine cache could not be read.');
  }
}

function writeCache(value: EngineCache) {
  localStorage.setItem(connection!.key, JSON.stringify(value));
}

function endpoint(url: string, token?: string) {
  const parsed = new URL(url);
  const local =
    parsed.hostname === 'localhost' ||
    parsed.hostname === '[::1]' ||
    /^127(?:\.\d{1,3}){3}$/.test(parsed.hostname);
  if (
    !['http:', 'https:'].includes(parsed.protocol) ||
    parsed.username ||
    parsed.password ||
    parsed.search ||
    parsed.hash
  )
    throw new EngineError(
      'input',
      'Use an HTTP(S) base URL without credentials, query or fragment.',
    );
  if (!local && (parsed.protocol !== 'https:' || !token))
    throw new EngineError('input', 'Remote engines require HTTPS and an access token.');
  if (token && (token.length > 8192 || /[\x00-\x1f\x7f]/.test(token)))
    throw new EngineError('input', 'Invalid access token.');
  return parsed.toString().replace(/\/$/, '');
}

function url(path: string[], cursor?: string | null) {
  if (!connection) throw new EngineError('disconnected', 'Connect to an engine in Settings first.');
  if (
    path.some(
      (part) =>
        !part || part === '.' || part === '..' || part.length > 256 || /[\x00-\x1f\x7f]/.test(part),
    )
  )
    throw new EngineError('input', 'Invalid API identifier.');
  const result = new URL(`${connection.url}/v1/${path.map(encodeURIComponent).join('/')}`);
  if (cursor) result.searchParams.set('cursor', cursor);
  return result;
}

async function readLimited(response: Response, max: number): Promise<Uint8Array> {
  if (Number(response.headers.get('content-length')) > max)
    throw new EngineError('limit', 'Engine response exceeds the client size limit.');
  const reader = response.body?.getReader();
  if (!reader) return new Uint8Array();
  let size = 0;
  const chunks: Uint8Array[] = [];
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      size += value.length;
      if (size > max)
        throw new EngineError('limit', 'Engine response exceeds the client size limit.');
      chunks.push(value);
    }
  } finally {
    await reader.cancel();
  }
  const result = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    result.set(chunk, offset);
    offset += chunk.length;
  }
  return result;
}

async function jsonRequest(
  path: string[],
  body?: unknown,
  operationId?: string,
  cursor?: string | null,
) {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (connection?.token) headers.Authorization = `Bearer ${connection.token}`;
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (operationId) headers['Idempotency-Key'] = operationId;
  const response = await fetch(url(path, cursor), {
    method: body === undefined ? 'GET' : 'POST',
    body: body === undefined ? undefined : JSON.stringify(body),
    headers,
    redirect: 'error',
    signal: AbortSignal.timeout(30_000),
    credentials: 'omit',
  });
  let value;
  try {
    value = JSON.parse(
      new TextDecoder('utf-8', { fatal: true }).decode(
        await readLimited(response, 96 * 1024 * 1024),
      ),
      (_, value) => {
        if (typeof value === 'number' && Number.isInteger(value) && !Number.isSafeInteger(value))
          throw new EngineError(
            'precision',
            'Engine returned an integer outside JavaScript’s safe range. The value was not accepted by the app.',
          );
        return value;
      },
    );
  } catch (e) {
    if (e instanceof EngineError) throw e;
    throw new EngineError('protocol', 'Engine returned invalid JSON.');
  }
  if (!response.ok)
    throw new EngineError(
      value.code ?? 'engine',
      value.message ?? 'Engine rejected the request.',
      response.status,
      value.diagnostics ?? [],
    );
  if (!value || typeof value !== 'object' || Array.isArray(value))
    throw new EngineError('protocol', 'Expected an engine response object.');
  return value;
}

export async function connectEngine(address: string, token?: string): Promise<EngineInfo> {
  await disconnectEngine();
  const revision = sessionRevision;
  const addressUrl = endpoint(address, token);
  try {
    let info: EngineInfo;
    if (desktop) info = await invoke('engine_connect', { url: addressUrl, token: token || null });
    else {
      connection = { url: addressUrl, token, key: '' };
      info = await jsonRequest(['info']);
    }
    if (
      info.protocol !== PROTOCOL ||
      typeof info.engineId !== 'string' ||
      typeof info.principalId !== 'string' ||
      !Array.isArray(info.capabilities)
    )
      throw new EngineError('incompatible', `Engine does not implement the ${PROTOCOL} contract.`);
    if (revision !== sessionRevision)
      throw new EngineError('disconnected', 'Connection changed while connecting.');
    connection = {
      url: addressUrl,
      token,
      key: `knotra.engine.v1:${JSON.stringify([addressUrl, info.engineId, info.principalId])}`,
    };
    return info;
  } catch (error) {
    if (revision === sessionRevision) connection = undefined;
    throw engineError(error);
  }
}

export async function disconnectEngine(): Promise<void> {
  sessionRevision++;
  for (const watch of watches) watch.abort();
  watches.clear();
  connection = undefined;
  if (desktop) await invoke('engine_disconnect');
}

export async function engineCache(): Promise<EngineCache> {
  if (!connection) return { cache: {}, pending: [] };
  return desktop ? invoke('engine_cache') : cache();
}

function route(call: EngineCall): { path: string[]; body?: unknown; cursor?: string | null } {
  switch (call.op) {
    case 'info':
    case 'definitions':
    case 'profiles':
    case 'resources':
      return { path: [call.op] };
    case 'runs':
    case 'requests':
    case 'artifacts':
      return { path: [call.op], cursor: call.cursor };
    case 'run':
      return { path: ['runs', call.runId] };
    case 'definition':
      return { path: ['definitions', call.definitionId] };
    case 'artifact':
      return { path: ['artifacts', call.artifactId] };
    case 'validate':
      return {
        path: ['packages', 'validate'],
        body: {
          package: call.package,
          profile: call.profile,
          inputs: call.inputs,
          artifacts: call.artifacts,
        },
      };
    case 'publish':
      return { path: ['definitions'], body: { package: call.package } };
    case 'start':
      return {
        path: ['runs'],
        body: {
          definitionId: call.definitionId,
          profile: call.profile,
          inputs: call.inputs,
          artifacts: call.artifacts,
        },
      };
    case 'cancel':
    case 'resume':
      return { path: ['runs', call.runId, call.op], body: {} };
    case 'resolve':
      return {
        path: ['runs', call.runId, 'instances', call.instanceId, 'resolve'],
        body: call.resolution,
      };
    case 'respond':
      return { path: ['requests', call.requestId, 'response'], body: { outputs: call.outputs } };
    case 'upload':
      return {
        path: ['artifacts'],
        body: { name: call.file.path, content: call.file.content, mediaType: call.mediaType },
      };
  }
}

function checkReceipt(call: EngineCall, value: any) {
  const id = (value: unknown) => typeof value === 'string' && !!value;
  const valid =
    call.op === 'publish'
      ? id(value.definition?.id)
      : call.op === 'start'
        ? id(value.run?.id)
        : call.op === 'respond'
          ? value.accepted === true && value.requestId === call.requestId
          : ['cancel', 'resume', 'resolve'].includes(call.op)
            ? value.accepted === true && value.runId === ('runId' in call ? call.runId : '')
            : call.op === 'upload'
              ? id(value.artifact?.id)
              : true;
  if (!valid)
    throw new EngineError(
      'protocol',
      'Engine returned an incompatible command receipt. Reconcile the saved operation.',
    );
}

export async function engineCall<T>(call: EngineCall): Promise<T> {
  if (!connection) throw new EngineError('disconnected', 'Connect to an engine in Settings first.');
  const revision = sessionRevision;
  if (desktop) {
    try {
      const result = await invoke<T>('engine_call', { call });
      if (revision !== sessionRevision)
        throw new EngineError(
          'disconnected',
          'Connection changed; recover commands on the original engine.',
        );
      return result;
    } catch (e) {
      throw engineError(e);
    }
  }
  const operationId = 'operationId' in call ? call.operationId : undefined;
  if (operationId) {
    // Serialize writes, including in-flight requests, so one ID cannot issue two different bodies.
    const task = browserWrites.then(async () => {
      if (revision !== sessionRevision)
        throw new EngineError('disconnected', 'Connection changed.');
      const data = cache();
      const old = data.pending.find((item) => item.id === operationId);
      const completed = data.cache[`operation:${operationId}`] as
        | {
            request: EngineCall;
            response?: T;
            rejection?: { code: string; message: string; status?: number; diagnostics: unknown[] };
          }
        | undefined;
      if (
        (old && stable(old.request) !== stable(call)) ||
        (completed && stable(completed.request) !== stable(call))
      )
        throw new EngineError('conflict', 'Operation ID already belongs to a different command.');
      if (completed?.rejection)
        throw new EngineError(
          completed.rejection.code,
          completed.rejection.message,
          completed.rejection.status,
          completed.rejection.diagnostics,
        );
      if (completed) return completed.response as T;
      if (!old) {
        data.pending.push({ id: operationId, request: call });
        writeCache(data);
      }
      const { path, body, cursor } = route(call);
      let response;
      try {
        response = await jsonRequest(path, body, operationId, cursor);
      } catch (error) {
        const caught = engineError(error);
        if (
          revision === sessionRevision &&
          caught.status &&
          caught.status >= 400 &&
          caught.status < 500 &&
          ![408, 429].includes(caught.status)
        ) {
          const current = cache();
          current.pending = current.pending.filter((item) => item.id !== operationId);
          current.cache[`operation:${operationId}`] = {
            request: call,
            rejection: {
              code: caught.code,
              message: caught.message,
              status: caught.status,
              diagnostics: caught.diagnostics,
            },
          };
          writeCache(current);
        }
        throw caught;
      }
      checkReceipt(call, response);
      if (revision !== sessionRevision)
        throw new EngineError(
          'disconnected',
          'Connection changed; recover the saved command on the original engine.',
        );
      const current = cache();
      current.pending = current.pending.filter((item) => item.id !== operationId);
      current.cache[`operation:${operationId}`] = { request: call, response };
      writeCache(current);
      return response as T;
    });
    browserWrites = task.then(
      () => {},
      () => {},
    );
    return task;
  }
  const { path, body, cursor } = route(call);
  const value = await jsonRequest(path, body, undefined, cursor);
  if (revision !== sessionRevision) throw new EngineError('disconnected', 'Connection changed.');
  if (call.op !== 'validate') {
    const current = cache();
    current.cache[JSON.stringify(call)] = value;
    writeCache(current);
  }
  return value as T;
}

export async function allPages<T>(op: 'runs' | 'requests' | 'artifacts'): Promise<T[]> {
  const items: T[] = [];
  let cursor: string | null = null;
  const seen = new Set<string>();
  do {
    const page: Page<T> = await engineCall({ op, cursor });
    if (
      !Array.isArray(page.items) ||
      !(page.nextCursor === null || typeof page.nextCursor === 'string')
    )
      throw new EngineError('protocol', 'Invalid paginated response.');
    items.push(...page.items);
    cursor = page.nextCursor;
    if (cursor && (seen.has(cursor) || seen.size >= 1000))
      throw new EngineError('protocol', 'Engine pagination did not finish.');
    if (cursor) seen.add(cursor);
  } while (cursor);
  return items;
}

export async function downloadArtifact(
  id: string,
): Promise<{ artifact: EngineArtifact; content: string }> {
  if (desktop) {
    try {
      return await invoke('engine_download', { artifactId: id });
    } catch (e) {
      throw engineError(e);
    }
  }
  const { artifact } = await engineCall<{ artifact: EngineArtifact }>({
    op: 'artifact',
    artifactId: id,
  });
  if (!Number.isSafeInteger(artifact.size) || artifact.size < 0 || artifact.size > 64 * 1024 * 1024)
    throw new EngineError('limit', 'Invalid artifact size or artifact exceeds 64 MiB.');
  const response = await fetch(url(['artifacts', id, 'content']), {
    headers: connection?.token ? { Authorization: `Bearer ${connection.token}` } : {},
    redirect: 'error',
    credentials: 'omit',
    signal: AbortSignal.timeout(60_000),
  });
  if (!response.ok)
    throw new EngineError('engine', 'Artifact bytes are unavailable.', response.status);
  const bytes = await readLimited(response, 64 * 1024 * 1024);
  if (bytes.length !== artifact.size || (await sha256(bytes)) !== artifact.sha256)
    throw new EngineError('integrity', 'Artifact size or SHA-256 does not match engine metadata.');
  return { artifact, content: base64(bytes) };
}

export async function exportEngineArtifact(id: string): Promise<void> {
  const { artifact, content } = await downloadArtifact(id);
  if (desktop) {
    await invoke('export_file', { name: artifact.name, content });
    return;
  }
  const bytes = Uint8Array.from(atob(content), (char) => char.charCodeAt(0));
  const blob = new Blob([bytes], { type: artifact.mediaType });
  const href = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = href;
  anchor.download = artifact.name;
  anchor.click();
  setTimeout(() => URL.revokeObjectURL(href), 1000);
}

export async function storedEvents(runId: string): Promise<EngineEvent[]> {
  if (desktop) return invoke('engine_events', { runId });
  return (cache().cache[`events:${runId}`] ?? []) as EngineEvent[];
}

export async function watchRun(
  runId: string,
  receive: (message: StreamMessage) => void,
): Promise<() => void> {
  if (desktop) {
    const channel = new Channel<StreamMessage>();
    channel.onmessage = receive;
    await invoke('engine_watch', { runId, channel });
    return () => {
      void invoke('engine_unwatch', { runId }).catch(() => {});
    };
  }
  const controller = new AbortController();
  const revision = sessionRevision;
  watches.add(controller);
  void (async () => {
    let delay = 1000;
    while (!controller.signal.aborted && revision === sessionRevision) {
      try {
        const data = cache();
        const cursor = data.cache[`cursor:${runId}`] as string | undefined;
        const headers: Record<string, string> = { Accept: 'text/event-stream' };
        if (cursor) headers['Last-Event-ID'] = cursor;
        if (connection?.token) headers.Authorization = `Bearer ${connection.token}`;
        const requestAbort = new AbortController();
        let heartbeat: ReturnType<typeof setTimeout> | undefined = setTimeout(
          () => requestAbort.abort(),
          15_000,
        );
        const response = await fetch(url(['runs', runId, 'events']), {
          headers,
          signal: AbortSignal.any([controller.signal, requestAbort.signal]),
          credentials: 'omit',
          redirect: 'error',
        }).finally(() => {
          clearTimeout(heartbeat);
        });
        if (
          !response.ok ||
          !response.headers.get('content-type')?.startsWith('text/event-stream') ||
          !response.body
        )
          throw new EngineError('protocol', 'Engine did not return an event stream.');
        receive({ type: 'connection', status: 'connected' });
        delay = 1000;
        const reader = response.body.getReader();
        const decoder = new TextDecoder('utf-8', { fatal: true });
        const parser = new EventParser();
        try {
          while (!controller.signal.aborted) {
            heartbeat = setTimeout(() => requestAbort.abort(), 45_000);
            const { value, done } = await reader.read().finally(() => clearTimeout(heartbeat));
            if (done || revision !== sessionRevision || controller.signal.aborted) break;
            for (const frame of parser.push(decoder.decode(value, { stream: true }))) {
              const event = JSON.parse(frame.data) as EngineEvent;
              if (event.runId !== runId || event.id !== frame.id)
                throw new EngineError('protocol', 'Event identity does not match its stream.');
              const current = cache();
              const events = (current.cache[`events:${runId}`] ?? []) as EngineEvent[];
              const ids = (current.cache[`eventIds:${runId}`] ?? []) as string[];
              if (!ids.includes(frame.id)) {
                current.cache[`events:${runId}`] = [...events, event].slice(-1000);
                current.cache[`eventIds:${runId}`] = [...ids, frame.id];
                current.cache[`cursor:${runId}`] = frame.id;
                writeCache(current);
                receive({ type: 'event', event });
              }
            }
          }
        } finally {
          await reader.cancel();
        }
      } catch (error) {
        if (!controller.signal.aborted)
          receive({
            type: 'connection',
            status: 'reconnecting',
            message: engineError(error).message,
          });
      }
      if (!controller.signal.aborted) {
        receive({ type: 'connection', status: 'reconnecting' });
        await new Promise<void>((resolve) => {
          const finish = () => {
            clearTimeout(timer);
            controller.signal.removeEventListener('abort', finish);
            resolve();
          };
          const timer = setTimeout(finish, delay);
          controller.signal.addEventListener('abort', finish, { once: true });
        });
        delay = Math.min(delay * 2, 15000);
      }
    }
  })();
  return () => {
    controller.abort();
    watches.delete(controller);
  };
}

export class EventParser {
  private line = '';
  private cr = false;
  private id = '';
  private data: string[] = [];
  private size = 0;
  push(text: string): { id: string; data: string }[] {
    const frames: { id: string; data: string }[] = [];
    for (const char of text) {
      if (char === '\n' && this.cr) {
        this.cr = false;
        continue;
      }
      this.cr = char === '\r';
      if (char === '\r' || char === '\n') {
        if (!this.line) {
          if (this.data.length) {
            if (!this.id)
              throw new EngineError('protocol', 'Engine event is missing a durable ID.');
            frames.push({ id: this.id, data: this.data.join('\n') });
          }
          this.id = '';
          this.data = [];
          this.size = 0;
        } else if (!this.line.startsWith(':')) {
          const split = this.line.indexOf(':');
          const field = split < 0 ? this.line : this.line.slice(0, split);
          const value = split < 0 ? '' : this.line.slice(split + 1).replace(/^ /, '');
          if (field === 'id') {
            if (value.length > 256 || /[\x00-\x1f\x7f]/.test(value))
              throw new EngineError('protocol', 'Invalid event ID.');
            this.id = value;
          }
          if (field === 'data') this.data.push(value);
        }
        this.line = '';
      } else this.line += char;
      const point = char.codePointAt(0)!;
      this.size += point <= 0x7f ? 1 : point <= 0x7ff ? 2 : point <= 0xffff ? 3 : 4;
      if (this.size > 256 * 1024) throw new EngineError('limit', 'Engine event exceeds 256 KiB.');
    }
    return frames;
  }
}
