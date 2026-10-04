import { parseDocument } from 'yaml';
import { validPath } from './validation';
import { sha256, textBytes, unbase64 } from './bytes';
import { invoke } from '@tauri-apps/api/core';
import { desktop } from './native';
import { examples, initialWorkspaces } from './examples';
import type { Theme } from './theme';
import { normalizeLocale, preferredLocale, type Locale } from './i18n';
import type { DemoRun, Workspace } from './types';

const KEY = 'knotra.workspace.v1';
const foldPath = (path: string) => path.replace(/[A-Z]/g, (letter) => letter.toLowerCase());

export interface State {
  version: 1;
  starterRevision?: 1 | 2 | 3;
  workspaces: Workspace[];
  runs: DemoRun[];
  activeId: string;
  engineUrl: string;
  compact: boolean;
  theme: Theme;
  locale: Locale;
}

export function freshState(): State {
  const locale = preferredLocale();
  const workspaces = initialWorkspaces(locale);
  return {
    version: 1,
    starterRevision: 3,
    workspaces,
    runs: [],
    activeId: workspaces[0].id,
    engineUrl: 'http://127.0.0.1:8787',
    compact: false,
    theme: 'dark',
    locale,
  };
}

export function loadState(): State {
  const raw = localStorage.getItem(KEY);
  return raw === null ? freshState() : readBackup(raw);
}

// Importing a backup is exact; only opening an older installation adds new starters.
// Once marked, deliberately deleted starters stay deleted on subsequent launches.
export async function migrateStarters(state: State): Promise<State> {
  if (state.starterRevision === 3) return state;
  const nameOf = (workspace: Workspace): string | undefined => {
    // A draft may temporarily fail contract validation while retaining its identity.
    if (workspace.source.length > 8 * 1024 * 1024) return;
    try {
      const name = parseDocument(workspace.source).getIn(['metadata', 'name']);
      return typeof name === 'string' ? name : undefined;
    } catch {
      return;
    }
  };
  const fingerprint = (source: string, files: Workspace['files']) =>
    sha256(
      textBytes(
        JSON.stringify([
          source,
          files
            .map((file) => [file.path, file.content])
            .sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0)),
        ]),
      ),
    );
  // Exact fingerprints of the three retired starter packages, including their
  // original EN/RU titles. Any source or supporting-file edit keeps the draft.
  const retired = new Set([
    '133fef130fb7f4032e6f4b41a308ff96041bca25cc2079adb1252e668ac4fbce',
    'f574889495ef6991b14627f4c181f191283bcda38304d59b02ed22be52f35800',
    '74a83e8f78aaf9c1a99b5c6e868c56866fe46b7295677775fc7c8a6ca9a396e9',
    'a7dd3e91dbd963ea16eaf74760e0f0ed31623d5b600b510a6d949cb01a18429e',
    '2104f88e6e59070380da9b7d29db3e0b20ecc8d1f41b0ea7371c89e1d4f7fe6a',
    'bee24c70d7afa6a95761dbe35a10d685d914da9b23a08d11c37622330a44e5de',
    'eba32eb49ed19e6f304f291567146d90edf8c5711cd46cae9af743cad17b5e94',
    '828d6f91f4961b7c8e154ac1f0918d1cd1d8ace9c187f816fc0dd77e457eced4',
    '1584077623c086da6b8c8bd0abda73b07841c5411681fa7a36000febae0427fd',
  ]);
  for (const example of examples.filter((example) =>
    ['research', 'agent', 'foreach', 'local'].includes(example.id),
  )) {
    retired.add(await fingerprint(example.source, example.files));
  }
  const retained: Workspace[] = [];
  for (const workspace of state.workspaces) {
    if (
      workspace.entrypoint !== 'pipeline.yaml' ||
      Object.keys(workspace.positions ?? {}).length > 0 ||
      workspace.source !== workspace.savedSource ||
      !retired.has(await fingerprint(workspace.source, workspace.files))
    ) {
      retained.push(workspace);
    }
  }
  const names = new Set(retained.map(nameOf));
  const additions = initialWorkspaces(state.locale).filter((workspace) => {
    const name = nameOf(workspace);
    return name && !names.has(name);
  });
  const workspaces = [...retained, ...additions];
  return {
    ...state,
    starterRevision: 3,
    workspaces,
    activeId: workspaces.some((w) => w.id === state.activeId)
      ? state.activeId
      : (workspaces[0]?.id ?? ''),
  };
}

let writes: Promise<void> = Promise.resolve();
let initialLoad: Promise<State> | undefined;

export function loadWorkspace(): Promise<State> {
  return (initialLoad ??= readWorkspace());
}

async function readWorkspace(): Promise<State> {
  if (!desktop) {
    const loaded = loadState();
    const migrated = await migrateStarters(loaded);
    if (migrated !== loaded) await saveState(migrated);
    return migrated;
  }
  const saved = await invoke<State | null>('workspace_load');
  if (saved) {
    const loaded = readBackup(JSON.stringify(saved));
    const migrated = await migrateStarters(loaded);
    if (migrated !== loaded) await saveState(migrated);
    return migrated;
  }
  const legacy = localStorage.getItem(KEY);
  const migrated = await migrateStarters(legacy === null ? freshState() : readBackup(legacy));
  await invoke('workspace_save', { value: migrated });
  return migrated;
}

export function saveState(state: State): Promise<void> {
  if (!desktop) {
    try {
      localStorage.setItem(KEY, JSON.stringify(state));
      return Promise.resolve();
    } catch (e) {
      return Promise.reject(e);
    }
  }
  const value = JSON.parse(JSON.stringify(state));
  const next = writes.catch(() => {}).then(() => invoke<void>('workspace_save', { value }));
  writes = next;
  return next;
}

export function flushWorkspace(): Promise<void> {
  return writes;
}

export function readBackup(text: string): State {
  if (new TextEncoder().encode(text).length > 96 * 1024 * 1024)
    throw new Error('Workspace backup exceeds 96 MiB.');
  const value = JSON.parse(text);
  if (
    !value ||
    value.version !== 1 ||
    !Array.isArray(value.workspaces) ||
    !Array.isArray(value.runs)
  )
    throw new Error('Unsupported workspace backup.');
  const ids = new Set<string>();
  for (const workspace of value.workspaces) {
    if (
      !workspace ||
      typeof workspace.id !== 'string' ||
      !workspace.id ||
      ids.has(workspace.id) ||
      typeof workspace.source !== 'string' ||
      typeof workspace.savedSource !== 'string' ||
      typeof workspace.entrypoint !== 'string' ||
      !Array.isArray(workspace.files) ||
      typeof workspace.updatedAt !== 'string'
    )
      throw new Error('Invalid pipeline draft in backup.');
    ids.add(workspace.id);
    if (
      !workspace.files.every(
        (file: unknown) =>
          file &&
          typeof file === 'object' &&
          typeof (file as { path: unknown }).path === 'string' &&
          typeof (file as { content: unknown }).content === 'string',
      )
    )
      throw new Error('Invalid supporting file in backup.');
    const paths = new Set([foldPath(workspace.entrypoint)]);
    let size = new TextEncoder().encode(workspace.source).length;
    if (!validPath(workspace.entrypoint) || workspace.files.length > 511)
      throw new Error('Invalid package paths or file count in backup.');
    for (const file of workspace.files) {
      if (!validPath(file.path) || paths.has(foldPath(file.path)))
        throw new Error('Duplicate or invalid package path in backup.');
      paths.add(foldPath(file.path));
      size += unbase64(file.content).length;
    }
    if (size > 64 * 1024 * 1024) throw new Error('Package in backup exceeds 64 MiB.');
    if (
      workspace.positions &&
      !Object.values(workspace.positions).every(
        (position) =>
          position &&
          typeof position === 'object' &&
          Number.isFinite((position as { x: number }).x) &&
          Number.isFinite((position as { y: number }).y),
      )
    )
      throw new Error('Invalid canvas positions in backup.');
  }
  const runIds = new Set<string>();
  for (const run of value.runs) {
    if (
      !run ||
      !['id', 'workspaceId', 'title', 'topic', 'createdAt', 'updatedAt', 'source'].every(
        (key) => typeof run[key] === 'string',
      ) ||
      runIds.has(run.id) ||
      !['running', 'waiting_human', 'succeeded', 'failed', 'cancelled'].includes(run.status) ||
      !Array.isArray(run.files) ||
      !Array.isArray(run.events) ||
      !Array.isArray(run.artifacts) ||
      !run.nodes ||
      !run.inputs ||
      !run.outputs
    )
      throw new Error('Invalid demo history in backup.');
    if (
      !run.events.every(
        (event: any) =>
          event && ['id', 'at', 'message'].every((key) => typeof event[key] === 'string'),
      ) ||
      !run.artifacts.every(
        (artifact: any) =>
          artifact &&
          ['id', 'name', 'content', 'sha256', 'mediaType'].every(
            (key) => typeof artifact[key] === 'string',
          ) &&
          Number.isSafeInteger(artifact.size) &&
          artifact.size >= 0,
      )
    )
      throw new Error('Invalid demo outputs or events in backup.');
    runIds.add(run.id);
  }
  const engineUrl = typeof value.engineUrl === 'string' ? value.engineUrl : 'http://127.0.0.1:8787';
  const address = new URL(engineUrl);
  if (
    !['http:', 'https:'].includes(address.protocol) ||
    address.username ||
    address.password ||
    address.search ||
    address.hash
  )
    throw new Error('Invalid engine address in backup.');
  return {
    version: 1,
    ...([1, 2, 3].includes(value.starterRevision)
      ? { starterRevision: value.starterRevision as 1 | 2 | 3 }
      : {}),
    workspaces: value.workspaces,
    runs: value.runs,
    activeId: ids.has(value.activeId) ? value.activeId : (value.workspaces[0]?.id ?? ''),
    engineUrl,
    compact: value.compact === true,
    theme: value.theme === 'light' ? 'light' : 'dark',
    locale: normalizeLocale(value.locale),
  };
}
