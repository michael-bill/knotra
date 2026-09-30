import { validPath } from './validation';
import { unbase64 } from './bytes';
import { invoke } from '@tauri-apps/api/core';
import { desktop } from './native';
import { initialWorkspaces } from './examples';
import type { Theme } from './theme';
import type { DemoRun, Workspace } from './types';
const KEY = 'knotra.workspace.v1';
export interface State { version: 1; workspaces: Workspace[]; runs: DemoRun[]; activeId: string; engineUrl: string; compact: boolean; theme: Theme }
export function freshState(): State { const workspaces = initialWorkspaces(); return { version: 1, workspaces, runs: [], activeId: workspaces[0].id, engineUrl: 'http://127.0.0.1:8080', compact: false, theme: 'dark' }; }
export function loadState(): State {
  try {
    const raw = localStorage.getItem(KEY); if (!raw) return freshState();
    const value = JSON.parse(raw) as State;
    if (value.version !== 1 || !Array.isArray(value.workspaces) || !Array.isArray(value.runs)) throw new Error('Unsupported workspace data.');
    if (!value.workspaces.every(w => typeof w.id === 'string' && typeof w.source === 'string' && typeof w.entrypoint === 'string' && Array.isArray(w.files))) throw new Error('Invalid workspace data.');
    return { ...value, theme: value.theme === 'light' ? 'light' : 'dark' };
  } catch { return freshState(); }
}
let writes: Promise<void> = Promise.resolve();
let initialLoad: Promise<State> | undefined;
export function loadWorkspace(): Promise<State> { return initialLoad ??= readWorkspace(); }
async function readWorkspace(): Promise<State> {
  if (!desktop) return loadState();
  const saved = await invoke<State | null>('workspace_load');
  if (saved) return readBackup(JSON.stringify(saved));
  const legacy = localStorage.getItem(KEY); const migrated = legacy ? readBackup(legacy) : freshState(); await invoke('workspace_save', { value: migrated }); return migrated;
}
export function saveState(state: State): Promise<void> {
  if (!desktop) { try { localStorage.setItem(KEY, JSON.stringify(state)); return Promise.resolve(); } catch (e) { return Promise.reject(e); } }
  const value = JSON.parse(JSON.stringify(state));
  const next = writes.catch(() => {}).then(() => invoke<void>('workspace_save', { value })); writes = next; return next;
}
export function flushWorkspace(): Promise<void> { return writes; }

export function readBackup(text: string): State {
  if (new TextEncoder().encode(text).length > 96 * 1024 * 1024) throw new Error('Workspace backup exceeds 96 MiB.');
  const value = JSON.parse(text);
  if (!value || value.version !== 1 || !Array.isArray(value.workspaces) || !Array.isArray(value.runs)) throw new Error('Unsupported workspace backup.');
  const ids = new Set<string>();
  for (const workspace of value.workspaces) {
    if (!workspace || typeof workspace.id !== 'string' || !workspace.id || ids.has(workspace.id) || typeof workspace.source !== 'string' || typeof workspace.savedSource !== 'string' || typeof workspace.entrypoint !== 'string' || !Array.isArray(workspace.files) || typeof workspace.updatedAt !== 'string') throw new Error('Invalid pipeline draft in backup.');
    ids.add(workspace.id);
    if (!workspace.files.every((file: unknown) => file && typeof file === 'object' && typeof (file as {path: unknown}).path === 'string' && typeof (file as {content: unknown}).content === 'string')) throw new Error('Invalid supporting file in backup.');
    const paths = new Set([workspace.entrypoint.toLowerCase()]); let size = new TextEncoder().encode(workspace.source).length;
    if (!validPath(workspace.entrypoint) || workspace.files.length > 511) throw new Error('Invalid package paths or file count in backup.');
    for (const file of workspace.files) { if (!validPath(file.path) || paths.has(file.path.toLowerCase())) throw new Error('Duplicate or invalid package path in backup.'); paths.add(file.path.toLowerCase()); size += unbase64(file.content).length; }
    if (size > 64 * 1024 * 1024) throw new Error('Package in backup exceeds 64 MiB.');
    if (workspace.positions && !Object.values(workspace.positions).every(position => position && typeof position === 'object' && Number.isFinite((position as {x: number}).x) && Number.isFinite((position as {y: number}).y))) throw new Error('Invalid canvas positions in backup.');
  }
  const runIds = new Set<string>();
  for (const run of value.runs) {
    if (!run || !['id','workspaceId','title','topic','createdAt','updatedAt','source'].every(key => typeof run[key] === 'string') || runIds.has(run.id) || !['running','waiting_human','succeeded','failed','cancelled'].includes(run.status) || !Array.isArray(run.files) || !Array.isArray(run.events) || !Array.isArray(run.artifacts) || !run.nodes || !run.inputs || !run.outputs) throw new Error('Invalid demo history in backup.');
    if (!run.events.every((event: any) => event && ['id','at','message'].every(key => typeof event[key] === 'string')) || !run.artifacts.every((artifact: any) => artifact && ['id','name','content','sha256','mediaType'].every(key => typeof artifact[key] === 'string') && Number.isSafeInteger(artifact.size) && artifact.size >= 0)) throw new Error('Invalid demo outputs or events in backup.');
    runIds.add(run.id);
  }
  const engineUrl = typeof value.engineUrl === 'string' ? value.engineUrl : 'http://127.0.0.1:8080';
  const address = new URL(engineUrl); if (!['http:', 'https:'].includes(address.protocol) || address.username || address.password || address.search || address.hash) throw new Error('Invalid engine address in backup.');
  return { version: 1, workspaces: value.workspaces, runs: value.runs, activeId: ids.has(value.activeId) ? value.activeId : value.workspaces[0]?.id ?? '', engineUrl, compact: value.compact === true, theme: value.theme === 'light' ? 'light' : 'dark' };
}
