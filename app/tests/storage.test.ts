import { afterEach, describe, expect, it, vi } from 'vitest';
import { parseDocument } from 'yaml';
import { freshState, loadState, migrateStarters, readBackup, type State } from '../src/lib/storage';
import { examples, fromExample } from '../src/lib/examples';
import { translate } from '../src/lib/i18n';
import { createDemoRun } from '../src/lib/demo';

const key = 'knotra.workspace.v1';
afterEach(() => vi.unstubAllGlobals());

describe('workspace loading and starter migration', () => {
  it.each(['{broken', '', JSON.stringify({ version: 1, workspaces: [{}], runs: [] })])(
    'rejects corrupt browser data without overwriting the original bytes: %s',
    (raw) => {
      const data = new Map([[key, raw]]);
      const setItem = vi.fn((name, value) => data.set(name, value));
      vi.stubGlobal('localStorage', { getItem: (name: string) => data.get(name) ?? null, setItem });
      expect(() => loadState()).toThrow();
      expect(data.get(key)).toBe(raw);
      expect(setItem).not.toHaveBeenCalled();
    },
  );

  it('creates exactly four starters in the preferred locale and marks them installed', () => {
    vi.stubGlobal('navigator', { language: 'ru-RU' });
    const state = freshState();
    expect(state.locale).toBe('ru');
    expect(state.starterRevision).toBe(3);
    const starters = examples.filter((example) => example.category === 'starter');
    expect(state.workspaces).toHaveLength(4);
    expect(
      state.workspaces.map((workspace) =>
        parseDocument(workspace.source).getIn(['metadata', 'title']),
      ),
    ).toEqual(starters.map((example) => translate(example.titleKey, 'ru')));
    expect(readBackup(JSON.stringify(state))).toEqual(state);
  });

  it('adds missing starters once without overwriting modified drafts or demo histories', async () => {
    const state = freshState();
    delete state.starterRevision;
    const edited = state.workspaces[0];
    edited.source =
      edited.source.replace('kind: Pipeline', 'kind: WorkInProgress') +
      '\n# unsaved user changes\n';
    const demo = fromExample(examples.find((example) => example.id === 'research')!);
    state.runs = [createDemoRun(demo, 'Keep this history')];
    demo.source += '\n# Custom saved research instructions\n';
    demo.savedSource = demo.source;
    state.workspaces = [edited, demo];
    const source = edited.source;
    const migrated = await migrateStarters(state);
    expect(migrated.starterRevision).toBe(3);
    expect(migrated.workspaces).toHaveLength(5);
    expect(migrated.workspaces.slice(0, 2)).toEqual([edited, demo]);
    expect(migrated.workspaces[0]).toBe(edited);
    expect(migrated.workspaces[0].source).toBe(source);
    expect(migrated.runs).toBe(state.runs);
    expect(migrated.activeId).toBe(state.activeId);
    expect(await migrateStarters(migrated)).toBe(migrated);
    expect(state.workspaces).toHaveLength(2);
    const deleted: State = { ...migrated, workspaces: [] };
    expect((await migrateStarters(deleted)).workspaces).toEqual([]);
  });

  it('imports a legacy backup exactly and defers migration until installation loading', async () => {
    const state = freshState();
    delete state.starterRevision;
    state.workspaces = [];
    state.activeId = '';
    const raw = JSON.stringify(state);
    const restored = readBackup(raw);
    expect(restored).toEqual(state);
    expect(restored.starterRevision).toBeUndefined();
    vi.stubGlobal('localStorage', { getItem: () => raw });
    expect(loadState()).toEqual(state);
    expect((await migrateStarters(restored)).workspaces).toHaveLength(4);
  });

  it.each([1, 2] as const)(
    'cleans only pristine legacy templates when upgrading revision %s',
    async (revision) => {
      const state = freshState();
      const existing = fromExample(examples.find((example) => example.id === 'research')!);
      state.starterRevision = revision;
      const edited = {
        ...existing,
        id: 'customized',
        source: existing.source + '\n# Personal changes\n',
      };
      edited.savedSource = edited.source;
      const history = createDemoRun(existing, 'Historical execution must remain');
      state.runs = [history];
      state.workspaces = [existing, edited];
      state.activeId = existing.id;
      const migrated = await migrateStarters(state);
      expect(migrated.starterRevision).toBe(3);
      expect(migrated.workspaces).toHaveLength(5);
      expect(migrated.workspaces[0]).toBe(edited);
      expect(migrated.workspaces.some((workspace) => workspace.id === existing.id)).toBe(false);
      expect(migrated.activeId).toBe(edited.id);
      expect(migrated.runs).toEqual([history]);
      expect(readBackup(JSON.stringify(state)).starterRevision).toBe(revision);
      expect(await migrateStarters(migrated)).toBe(migrated);
    },
  );
});

it('keeps legacy drafts with a customized package name, layout, or supporting files', async () => {
  const original = fromExample(examples.find((example) => example.id === 'research')!);
  const customized = [
    { ...original, id: 'entrypoint', entrypoint: 'custom.yaml' },
    { ...original, id: 'layout', positions: { research: { x: 120, y: 320 } } },
    { ...original, id: 'files', files: [{ path: 'notes.txt', content: 'bm90ZXM=' }] },
  ];
  const state = { ...freshState(), starterRevision: 2 as const, workspaces: customized };
  const migrated = await migrateStarters(state);
  expect(migrated.workspaces.slice(0, 3)).toEqual(customized);
  expect(migrated.workspaces).toHaveLength(7);
});
