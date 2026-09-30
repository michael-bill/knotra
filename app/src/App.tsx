import { lazy, Suspense, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { parseDocument, stringify } from 'yaml';
import { zipSync, unzipSync } from 'fflate';
import { Workflow, History, Inbox, FileBox, Cable, Settings2, Search, Plus, ChevronRight, ArrowUpRight, FolderOpen, ArrowDownToLine, Check, X, Command, CircleHelp, ArrowRight, Monitor, Upload, Braces, Sparkles, Bell, PanelLeftClose, PanelLeftOpen } from 'lucide-react';
import { type Artifact, type DemoRun, type Json, type NodeKind, type Pipeline, type Section, type Workspace, record } from './lib/types';
import { examples, fromExample, RESEARCH_SOURCE } from './lib/examples';
import { advanceDemo, cancelDemo, createDemoRun, demoAvailable, finishDemo, respondDemo } from './lib/demo';
import { ThemeContext } from './lib/theme';
import { loadWorkspace, saveState, readBackup, type State } from './lib/storage';
import { parsePipeline, validatePipeline, validPath } from './lib/validation';
import { base64, decodeText, textBytes, unbase64 } from './lib/bytes';
import { browserPackage, desktop, download, exportFile, exportPackage, openPackage, type OpenedPackage } from './lib/native';
import { Modal, NodeIcon, nodeMeta } from './components/ui';
import { RunsView, InboxView, ArtifactsView } from './components/RunViews';
import { ConnectionsView, SettingsView } from './components/ResourceViews';
import { useEngine } from './lib/engine/useEngine';
import { EngineArtifactsView, EngineBanner, EngineInboxView, EngineRunDialog, EngineRunsView } from './components/EngineViews';
const WorkspaceView = lazy(() => import('./components/WorkspaceView'));
const navigation: { id: Section; title: string; icon: typeof Workflow }[] = [{ id: 'pipelines', title: 'Pipelines', icon: Workflow }, { id: 'runs', title: 'Runs', icon: History }, { id: 'inbox', title: 'Inbox', icon: Inbox }, { id: 'artifacts', title: 'Artifacts', icon: FileBox }, { id: 'connections', title: 'Resources', icon: Cable }];
type Dialog = 'library' | 'run' | 'node' | 'search' | 'delete' | 'import' | 'entrypoint' | 'engine-run' | 'restore' | undefined;
export default function App() {
  const [initial, setInitial] = useState<State>(); const [error, setError] = useState('');
  useEffect(() => { void loadWorkspace().then(setInitial).catch(error => setError(error instanceof Error ? error.message : String(error))); }, []);
  if (!initial) return <div className="loading" role={error ? 'alert' : 'status'}>{error || 'Opening local workspace…'}{error ? <button className="button" onClick={() => location.reload()}>Retry</button> : null}</div>;
  return <LoadedApp initial={initial} />;
}
function LoadedApp({ initial }: { initial: State }) {
  const [restoring, setRestoring] = useState<State>(); const backupInput = useRef<HTMLInputElement>(null);
  const [addScope, setAddScope] = useState<string[]>([]);
  const [openResources, setOpenResources] = useState<'models' | 'mcp' | 'sandboxes' | 'secrets'>();
  const [state, setState] = useState<State>(initial); const [section, setSection] = useState<Section>('pipelines'); const [dialog, setDialog] = useState<Dialog>(); const [toast, setToast] = useState(''); const [query, setQuery] = useState(''); const [topic, setTopic] = useState('Reliable AI workflows'); const [activeRun, setActiveRun] = useState<string>(); const [busy, setBusy] = useState(false); const [pendingPackage, setPendingPackage] = useState<OpenedPackage>(); const [entries, setEntries] = useState<string[]>([]); const [pickedEntry, setPickedEntry] = useState('');
  useLayoutEffect(() => { document.documentElement.dataset.theme = state.theme; }, [state.theme]);
  const fileInput = useRef<HTMLInputElement>(null); const folderInput = useRef<HTMLInputElement>(null); const stateRef = useRef(state); stateRef.current = state;
  const workspace = state.workspaces.find(w => w.id === state.activeId) ?? state.workspaces[0];
  const validation = useMemo(() => workspace ? validatePipeline(workspace.source, workspace.files, workspace.entrypoint) : { diagnostics: [] }, [workspace?.source, workspace?.files, workspace?.entrypoint]);
  const names = useMemo(() => new Map(state.workspaces.map(w => { const p = parsePipeline(w.source); return [w.id, p?.metadata.title ?? p?.metadata.name ?? 'Pipeline draft']; })), [state.workspaces]);
  const [runMode, setRunMode] = useState<'demo' | 'engine'>('demo');
  const closeDialog = useCallback(() => setDialog(undefined), []);
  const notify = useCallback((message: string) => setToast(message), []);
  useLayoutEffect(() => { document.querySelector('.main-content')?.scrollTo(0, 0); }, [section, runMode, activeRun]);
  const engine = useEngine(notify);
  const waiting = state.runs.filter(r => r.status === 'waiting_human').length + (engine.info ? engine.requests.filter(request => request.status === 'open').length : 0);
  useEffect(() => { if (engine.info) setRunMode('engine'); }, [engine.info]);
  useEffect(() => { const timer = setTimeout(() => { void saveState(state).catch(() => setToast('Workspace storage is unavailable. Export a backup to preserve your work.')); }, desktop ? 0 : 250); return () => clearTimeout(timer); }, [state]);
  useEffect(() => { const save = () => { void saveState(stateRef.current).catch(() => {}); }; window.addEventListener('beforeunload', save); return () => window.removeEventListener('beforeunload', save); }, []);
  useEffect(() => {
    if (!desktop) return; let release: (() => void) | undefined; let disposed = false;
    void import('@tauri-apps/api/window').then(async ({ getCurrentWindow }) => {
      const window = getCurrentWindow(); const unlisten = await window.onCloseRequested(async event => { event.preventDefault(); try { await saveState(stateRef.current); await window.destroy(); } catch { notify('Could not save the workspace. Export a backup before closing.'); } });
      if (disposed) unlisten(); else release = unlisten;
    }); return () => { disposed = true; release?.(); };
  }, [notify]);
  useEffect(() => { if (!toast) return; const timer = setTimeout(() => setToast(''), 6000); return () => clearTimeout(timer); }, [toast]);
  useEffect(() => {
    const timers = state.runs.filter(r => r.status === 'running').map(run => setTimeout(async () => {
      if (run.nodes.publish === 'running') {
        try { const finished = await finishDemo(run); setState(s => ({ ...s, runs: s.runs.map(current => current.id === run.id && current.status === 'running' && current.updatedAt === run.updatedAt ? finished : current) })); } catch (e) { notify(String(e)); }
      } else setState(s => ({ ...s, runs: s.runs.map(current => current.id === run.id && current.status === 'running' ? advanceDemo(current) : current) }));
    }, 1300));
    return () => timers.forEach(clearTimeout);
  }, [state.runs, notify]);
  const saveDraft = useCallback(() => { const w = stateRef.current.workspaces.find(w => w.id === stateRef.current.activeId); if (!w) return; setState(s => ({ ...s, workspaces: s.workspaces.map(current => current.id === w.id ? { ...current, savedSource: current.source } : current) })); notify('Draft saved in this local workspace.'); }, [notify]);
  useEffect(() => {
    const key = (e: KeyboardEvent) => { if (!(e.metaKey || e.ctrlKey)) return; if (e.key.toLowerCase() === 'k') { e.preventDefault(); setQuery(''); setDialog('search'); } if (e.key.toLowerCase() === 's') {
      e.preventDefault();
      const field = document.activeElement;
      if (field instanceof HTMLInputElement || field instanceof HTMLTextAreaElement) {
        const label = field.getAttribute('aria-label'); const start = field.selectionStart; const end = field.selectionEnd;
        field.blur();
        // Setup fields commit on blur. Save after React has applied that edit, preserving editor focus.
        requestAnimationFrame(() => {
          saveDraft();
          const next = field.isConnected ? field : [...document.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>('input, textarea')].find(input => label && input.getAttribute('aria-label') === label);
          next?.focus(); if (next && start !== null && end !== null) next.setSelectionRange(start, end);
        });
      } else saveDraft();
    } if (e.key.toLowerCase() === 'n') { e.preventDefault(); setDialog('library'); } };
    window.addEventListener('keydown', key); return () => window.removeEventListener('keydown', key);
  }, [saveDraft]);
  function selectWorkspace(id: string) { setOpenResources(undefined); setState(s => ({ ...s, activeId: id })); setSection('pipelines'); closeDialog(); }
  function addWorkspace(opened: OpenedPackage) {
    setOpenResources(undefined);
    const w: Workspace = { id: crypto.randomUUID(), source: opened.source, savedSource: opened.source, files: opened.files, entrypoint: opened.entrypoint, updatedAt: new Date().toISOString() };
    setState(s => ({ ...s, workspaces: [...s.workspaces, w], activeId: w.id })); setSection('pipelines'); closeDialog(); notify('Pipeline package added to your local workspace.');
  }
  function selectExample(id: string) { setOpenResources(undefined); const example = examples.find(e => e.id === id)!; const w = fromExample(example); setState(s => ({ ...s, workspaces: [...s.workspaces, w], activeId: w.id })); setSection('pipelines'); closeDialog(); }
  async function importNative() { setBusy(true); try { const opened = await openPackage(); if (opened) addWorkspace(opened); } catch (e) { notify(String(e)); } finally { setBusy(false); } }
  function allowedFiles(opened: OpenedPackage): OpenedPackage {
    const files = new Map(opened.files.map(f => [f.path, f])); const declared = new Set<string>(); const visited = new Set<string>();
    function walk(source: string, path: string) { if (visited.has(path)) return; visited.add(path); if (visited.size > 512) throw new Error('Too many package files.'); const p = parsePipeline(source); if (!p) return; for (const name of p.spec.files ?? []) { declared.add(name); if (/\.ya?ml$/.test(name) && files.has(name)) { try { walk(decodeText(files.get(name)!.content), name); } catch { /* Validation will report invalid text/imports. */ } } } }
    walk(opened.source, opened.entrypoint); return { ...opened, files: opened.files.filter(f => declared.has(f.path)) };
  }
  async function importBrowser(files: File[]) {
    if (!files.length) return; setBusy(true);
    try {
      let opened: OpenedPackage;
      if (files.length === 1 && files[0].name.endsWith('.zip')) {
        if (files[0].size > 64 * 1024 * 1024) throw new Error('Archive exceeds 64 MiB.');
        let size = 0; let count = 0;
        const bytes = unzipSync(new Uint8Array(await files[0].arrayBuffer()), { filter: file => { if (file.name.endsWith('/')) return false; size += file.originalSize; count++; if (count > 512 || size > 64 * 1024 * 1024 || !validPath(file.name)) throw new Error('Archive exceeds package limits or contains an invalid path.'); return true; } });
        const packageFiles = Object.entries(bytes).map(([path, content]) => ({ path, content: base64(content) })); const candidates = packageFiles.filter(f => /\.ya?ml$/.test(f.path)); if (!candidates.length) throw new Error('Archive has no YAML entrypoint.');
        opened = { entrypoint: candidates[0].path, source: decodeText(candidates[0].content), files: packageFiles.filter(f => f.path !== candidates[0].path) };
      } else {
        if (files.length > 512 || files.reduce((n, f) => n + f.size, 0) > 64 * 1024 * 1024) throw new Error('Selection exceeds 512 files or 64 MiB.');
        const first = files.find(f => /\.ya?ml$/.test(f.name)); if (!first) throw new Error('Select a YAML pipeline or a ZIP package.'); const entrypoint = first.webkitRelativePath ? first.webkitRelativePath.split('/').slice(1).join('/') : first.name;
        opened = await browserPackage(files, entrypoint);
      }
      const all = [{ path: opened.entrypoint, content: base64(textBytes(opened.source)) }, ...opened.files]; const candidates = all.filter(f => /\.ya?ml$/.test(f.path) && (() => { try { return Boolean(parsePipeline(decodeText(f.content))); } catch { return false; } })()).map(f => f.path);
      if (candidates.length > 1) { setPendingPackage(opened); setEntries(candidates); setPickedEntry(candidates.includes('pipeline.yaml') ? 'pipeline.yaml' : candidates[0]); setDialog('entrypoint'); } else addWorkspace(allowedFiles(opened));
    } catch (e) { notify(e instanceof Error ? e.message : String(e)); } finally { setBusy(false); }
  }
  function confirmEntry() { if (!pendingPackage) return; const all = [{ path: pendingPackage.entrypoint, content: base64(textBytes(pendingPackage.source)) }, ...pendingPackage.files]; const file = all.find(f => f.path === pickedEntry)!; addWorkspace(allowedFiles({ entrypoint: pickedEntry, source: decodeText(file.content), files: all.filter(f => f.path !== pickedEntry) })); setPendingPackage(undefined); }
  async function exportCurrent() {
    if (!workspace) return; setBusy(true);
    try {
      if (validation.diagnostics.some(d => d.severity === 'error')) throw new Error('Resolve the local validation errors before exporting a runnable package. You can still save the draft or export a workspace backup.');
      const name = validation.pipeline?.metadata.name ?? 'pipeline'; const declared = allowedFiles({ entrypoint: workspace.entrypoint, source: workspace.source, files: workspace.files });
      if (desktop) { const path = await exportPackage(declared.entrypoint, declared.source, declared.files, name); if (path) notify(`Package exported to ${path}`); }
      else {
        const entries = Object.fromEntries([{ path: workspace.entrypoint, content: base64(textBytes(workspace.source)) }, ...declared.files].map(f => [f.path, unbase64(f.content)])); const zipped = zipSync(entries); const url = URL.createObjectURL(new Blob([Uint8Array.from(zipped).buffer], { type: 'application/zip' })); const a = document.createElement('a'); a.href = url; a.download = name + '.zip'; a.click(); setTimeout(() => URL.revokeObjectURL(url), 1000); notify('Complete pipeline package exported as ZIP.');
      }
    } catch (e) { notify(e instanceof Error ? e.message : String(e)); } finally { setBusy(false); }
  }
  function startDialog() {
    setRunMode('demo');
    const demo = state.workspaces.find(demoAvailable);
    if (!demo) { const w = fromExample(examples[0]); setState(s => ({ ...s, workspaces: [...s.workspaces, w], activeId: w.id })); } else setState(s => ({ ...s, activeId: demo.id }));
    setDialog('run');
  }
  function startRun() { if (!workspace) return; try { const run = createDemoRun(workspace, topic); setState(s => ({ ...s, runs: [run, ...s.runs] })); setActiveRun(run.id); setSection('runs'); closeDialog(); } catch (e) { notify(String(e)); } }
  function respond(id: string, request: string, response: Record<string, Json>) {
    const current = state.runs.find(r => r.id === id); if (!current) throw new Error('Run no longer exists.'); const updated = respondDemo(current, request, response);
    setState(s => ({ ...s, runs: s.runs.map(run => run.id === id && run.status === 'waiting_human' && run.requestId === request ? updated : run) })); notify('Review response accepted. The demo is continuing.');
  }
  function addNode(kind: NodeKind) {
    if (!workspace || !validation.pipeline) return;
    const example = examples.find(e => e.id === (kind === 'pipeline' ? 'subpipeline' : kind)); if (!example) return;
    const p = parsePipeline(example.source)!; const node = structuredClone(Object.values(p.spec.nodes)[0]); const current = validation.pipeline;
    const doc = parseDocument(workspace.source); const path = ['spec']; let graph = current.spec as import('./lib/types').Graph;
    for (const parent of addScope) { const outer = graph.nodes[parent]; if (!outer || !['foreach', 'loop'].includes(outer.type)) { notify('The selected body is no longer available.'); return; } path.push('nodes', parent, outer.type, 'body'); graph = record(record(outer[outer.type]).body) as unknown as import('./lib/types').Graph; }
    let id: string = kind; let i = 2; while (id in graph.nodes) id = `${kind}_${i++}`;
    // Also copy resources and graph inputs used by the contract template, without overwriting existing aliases.
    for (const key of ['models', 'mcp', 'sandboxes', 'secrets', 'schemas'] as const) if (p.spec[key]) doc.setIn(['spec', key], { ...p.spec[key], ...current.spec[key] });
    if (p.spec.inputs) doc.setIn([...path, 'inputs'], { ...p.spec.inputs, ...graph.inputs });
    for (const port of Object.values(node.inputs ?? {})) if (port.bind?.from?.startsWith('nodes.')) { const schema = record(port.schema); port.bind = { value: schema.type === 'integer' ? 0 : schema.type === 'array' ? [] : schema.type === 'object' ? {} : 'Example input' }; }
    doc.setIn([...path, 'nodes', id], node);
    const additions = example.files.filter(f => !workspace.files.some(existing => existing.path === f.path));
    if (additions.length) doc.setIn(['spec', 'files'], [...new Set([...(current.spec.files ?? []), ...(p.spec.files ?? [])])]);
    setState(s => ({ ...s, workspaces: s.workspaces.map(w => w.id === workspace.id ? { ...w, source: doc.toString(), files: [...w.files, ...additions], updatedAt: new Date().toISOString() } : w) })); closeDialog(); notify(`${nodeMeta[kind].label} node added. Choose its model or tool in Setup, then connect its inputs.`);
  }
  async function exportArtifact(artifact: Artifact) { try { await exportFile(artifact.name, artifact.content, artifact.mediaType); } catch (e) { notify(String(e)); } }
  function navigate(next: Section) { setOpenResources(undefined); setSection(next); if (next === 'runs') setActiveRun(undefined); }
  function openImport() { if (desktop) void importNative(); else setDialog('import'); }
  return <ThemeContext.Provider value={state.theme}><div className={`app-shell ${state.compact ? 'compact' : ''}`}>
    <aside className="app-sidebar"><div className="sidebar-brand-row"><button className="brand" aria-label="Knotra workspace" onClick={() => navigate('pipelines')}><img src="/knotra.svg" alt="" /><span>knotra</span></button><button className="icon-button nav-toggle" aria-label={state.compact ? 'Expand navigation' : 'Collapse navigation'} aria-expanded={!state.compact} title={state.compact ? 'Expand navigation' : 'Collapse navigation'} onClick={() => setState(s => ({ ...s, compact: !s.compact }))}>{state.compact ? <PanelLeftOpen size={17} /> : <PanelLeftClose size={17} />}</button></div><button className="sidebar-search" onClick={() => { setQuery(''); setDialog('search'); }}><Search size={16} /><span>Find anything</span><kbd>⌘ K</kbd></button><nav aria-label="Main navigation">{navigation.map(({ id, title, icon: Icon }) => <button key={id} aria-label={title} aria-current={section === id ? 'page' : undefined} className={section === id ? 'active' : ''} onClick={() => navigate(id)} title={title}><Icon size={18} /><span>{title}</span>{id === 'inbox' && waiting ? <i className="nav-alert">{waiting}</i> : null}</button>)}</nav><div className="sidebar-section"><span>YOUR PIPELINES</span><button className="icon-button" aria-label="Create pipeline" onClick={() => setDialog('library')}><Plus size={15} /></button></div><div className="sidebar-pipelines">{state.workspaces.map(w => <button className={workspace?.id === w.id && section === 'pipelines' ? 'selected' : ''} key={w.id} onClick={() => selectWorkspace(w.id)} title={names.get(w.id)}><span className="pipeline-dot" /><span>{names.get(w.id)}</span>{w.source !== w.savedSource ? <i className="unsaved-dot" /> : null}</button>)}</div><button className="sidebar-create" onClick={() => setDialog('library')}><Plus size={16} /><span>New pipeline</span></button><div className="sidebar-bottom"><button className={section === 'settings' ? 'active' : ''} onClick={() => navigate('settings')} title="Workspace settings"><Settings2 size={18} /><span>Settings</span></button><div className="sidebar-environment"><span className="dot green" /><span>Local workspace<small>{engine.info ? 'Engine connected' : 'Offline authoring'}</small></span></div></div></aside>
    <main className="main"><header className="app-header"><div className="workspace-location"><span>Personal workspace</span><ChevronRight size={13} /><strong>{section === 'connections' ? 'Resources' : section[0].toUpperCase() + section.slice(1)}</strong></div><div className="header-actions"><span className="runtime-label">{desktop ? 'Desktop' : 'Browser preview'}</span><button className="button" disabled={busy} onClick={openImport}><FolderOpen size={15} /><span>{busy ? 'Working…' : 'Open package'}</span></button><button className="icon-button notification-button" aria-label={waiting ? `${waiting} pending reviews` : 'Review inbox'} onClick={() => navigate('inbox')}><Bell size={18} />{waiting ? <i /> : null}</button></div></header>
    <div className={`main-content ${['runs', 'inbox', 'artifacts'].includes(section) ? 'execution-view' : ''}`}>{['runs', 'inbox', 'artifacts'].includes(section) ? <><div className="engine-mode-tabs underline-tabs" aria-label="Run mode"><button className={runMode === 'engine' ? 'active' : ''} onClick={() => setRunMode('engine')}>Engine</button><button className={runMode === 'demo' ? 'active' : ''} onClick={() => setRunMode('demo')}>Guided demo</button></div>{runMode === 'engine' ? <EngineBanner engine={engine} onSettings={() => navigate('settings')} /> : null}</> : null}{section === 'pipelines' ? <Suspense fallback={<div className="loading">Opening workspace…</div>}><WorkspaceView resourcesRequested={openResources} key={workspace?.id ?? 'empty'} workspace={workspace} validation={validation} engineConnected={!!engine.info && !engine.error} onEngineRun={() => setDialog('engine-run')} demo={workspace ? demoAvailable(workspace) : false} onChange={(source, files, positions) => { if (!workspace) return; setState(s => ({ ...s, workspaces: s.workspaces.map(w => w.id === workspace.id ? { ...w, source, files: files ?? w.files, positions: positions ?? w.positions, updatedAt: new Date().toISOString() } : w) })); }} onSave={saveDraft} onExport={() => void exportCurrent()} onRun={() => setDialog('run')} onLibrary={() => setDialog('library')} onAddNode={scope => { setAddScope(scope); setDialog('node'); }} onDelete={() => setDialog('delete')} onNotify={notify} /></Suspense> : section === 'runs' && runMode === 'engine' ? <EngineRunsView engine={engine} activeId={activeRun} onSelect={setActiveRun} onReview={() => setSection('inbox')} /> : section === 'runs' ? <RunsView runs={state.runs} activeId={activeRun} onSelect={setActiveRun} onCancel={id => setState(s => ({ ...s, runs: s.runs.map(r => r.id === id ? cancelDemo(r) : r) }))} onReview={id => { setActiveRun(id); setSection('inbox'); }} onStart={startDialog} /> : section === 'inbox' && runMode === 'engine' ? <EngineInboxView engine={engine} onRun={id => { setActiveRun(id); setSection('runs'); }} /> : section === 'inbox' ? <InboxView runs={state.runs} onRespond={respond} onRun={id => { setActiveRun(id); setSection('runs'); }} /> : section === 'artifacts' && runMode === 'engine' ? <EngineArtifactsView engine={engine} /> : section === 'artifacts' ? <ArtifactsView runs={state.runs} onExport={exportArtifact} /> : section === 'connections' ? <ConnectionsView engine={engine} pipeline={validation.pipeline} onSource={kind => { setOpenResources(kind); setSection('pipelines'); }} /> : <SettingsView onRestore={() => backupInput.current?.click()} engine={engine} key={state.engineUrl} engineUrl={state.engineUrl} desktop={desktop} theme={state.theme} onTheme={theme => setState(s => ({ ...s, theme }))} onSaveUrl={engineUrl => { if (engineUrl !== state.engineUrl) void engine.disconnect().catch(error => notify(String(error))); setState(s => ({ ...s, engineUrl })); }} onBackup={() => { void exportFile('knotra-workspace-backup.json', JSON.stringify(state, null, 2), 'application/json').catch(error => notify(String(error))); }} onNotify={notify} />}</div>
    </main>
    <input ref={backupInput} hidden type="file" accept=".json,application/json" aria-label="Restore workspace backup" onChange={async event => { const file = event.target.files?.[0]; event.target.value = ''; if (!file) return; try { if (file.size > 96 * 1024 * 1024) throw new Error('Workspace backup exceeds 96 MiB.'); setRestoring(readBackup(await file.text())); setDialog('restore'); } catch (error) { notify(error instanceof Error ? error.message : String(error)); } }} />
    {dialog === 'restore' && restoring ? <Modal title="Restore workspace backup?" subtitle="Review the backup before replacing your local authoring workspace." onClose={closeDialog}><div className="modal-body"><p>{restoring.workspaces.length} pipeline drafts · {restoring.runs.length} demo runs · {restoring.theme} theme</p><p>Your current drafts and demo history will be replaced. Engine execution and its command journal are preserved separately.</p></div><div className="modal-footer"><button className="button" onClick={closeDialog}>Keep current workspace</button><button className="button primary" onClick={async () => { try { await saveState(restoring); await engine.disconnect(); setState(restoring); setActiveRun(undefined); setSection('pipelines'); setRunMode('demo'); setRestoring(undefined); closeDialog(); notify('Workspace backup restored.'); } catch (error) { notify(error instanceof Error ? error.message : String(error)); } }}>Restore backup</button></div></Modal> : null}
    {dialog === 'engine-run'  && workspace && engine.info ? <EngineRunDialog workspace={{ ...workspace, ...allowedFiles(workspace) }} engine={engine} onClose={closeDialog} onStarted={id => { setActiveRun(id); setRunMode('engine'); setSection('runs'); }} /> : null}
    {dialog === 'library' ? <Modal title="Build your next workflow." subtitle="Start with a template, then connect the pieces." onClose={closeDialog} wide><div className="library-grid">{examples.map(e => <button key={e.id} className="template-card" onClick={() => selectExample(e.id)}><span className={`node-icon tint-${e.id === 'research' ? 'green' : 'violet'}`}>{e.id === 'research' ? <Workflow size={21} /> : <NodeIcon kind={(e.id === 'subpipeline' ? 'pipeline' : e.id === 'artifact-mount' ? 'code' : e.id) as NodeKind} size={21} />}</span><h3>{e.title}</h3><p>{e.description}</p><span className="template-kind">{e.kind}<ArrowUpRight size={14} /></span></button>)}</div><div className="modal-footer"><span>Examples follow the repository’s notation v1 fixtures.</span><button className="button" onClick={() => { const p = structuredClone(parsePipeline(examples.find(e => e.id === 'human')!.source)!); p.metadata = { name: 'untitled-pipeline', title: 'Untitled pipeline', description: 'Your next process starts here.' }; addWorkspace({ source: stringify(p), entrypoint: 'pipeline.yaml', files: [] }); }}><Plus size={15} />Start from scratch</button></div></Modal> : null}
    {dialog === 'node' ? <Modal title="Add a node" subtitle="Choose a block. Set it up in the right panel, then connect its named inputs and outputs." onClose={closeDialog} wide><div className="node-palette">{Object.entries(nodeMeta).map(([kind, meta]) => <button key={kind} onClick={() => addNode(kind as NodeKind)}><span className={`node-icon tint-${meta.color}`}><NodeIcon kind={kind as NodeKind} /></span><div><strong>{meta.label}</strong><span>{meta.detail}</span></div><Plus size={16} /></button>)}</div></Modal> : null}
    {dialog === 'run' ? <Modal title="Run your workflow" subtitle="A guided Research brief demo, with sample outputs." onClose={closeDialog}><div className="modal-body"><div className="notice"><Sparkles size={19} /><p>This demo simulates five steps, pauses for your review, and produces a sample Markdown file. It does not execute YAML or contact any services.</p></div><label className="field">Research topic<input autoFocus aria-label="Research topic" value={topic} onChange={e => setTopic(e.target.value)} placeholder="What would you like to explore?" onKeyDown={e => { if (e.key === 'Enter' && topic.trim()) startRun(); }} /></label><div className="run-steps">{['Discover', 'Research', 'Draft', 'Review', 'Publish'].map((name, i) => <span key={name}><em>{i + 1}</em>{name}{i < 4 ? <ChevronRight size={12} /> : null}</span>)}</div></div><div className="modal-footer"><span>Each run preserves its own source snapshot.</span><button className="button primary" onClick={startRun} disabled={!topic.trim()}><ArrowRight size={15} />Start demo</button></div></Modal> : null}
    {dialog === 'search' ? <Modal title="Jump to anything" onClose={closeDialog}><div className="command-search"><Search size={19} /><input autoFocus aria-label="Search workspace" placeholder="Search pipelines or views…" value={query} onChange={e => setQuery(e.target.value)} /></div><div className="command-results">{state.workspaces.filter(w => names.get(w.id)?.toLowerCase().includes(query.toLowerCase())).map(w => <button key={w.id} onClick={() => selectWorkspace(w.id)}><Workflow size={17} /><span>{names.get(w.id)}</span><small>Pipeline</small></button>)}{[...navigation, { id: 'settings' as const, title: 'Settings', icon: Settings2 }].filter(n => n.title.toLowerCase().includes(query.toLowerCase())).map(({ id, title, icon: Icon }) => <button key={id} onClick={() => { navigate(id); closeDialog(); }}><Icon size={17} /><span>{title}</span><small>View</small></button>)}</div><div className="modal-footer"><span>⌘/Ctrl K · Find<span className="separator">/</span>⌘/Ctrl S · Save<span className="separator">/</span>⌘/Ctrl N · Create</span></div></Modal> : null}
    {dialog === 'delete' ? <Modal title="Remove this pipeline?" onClose={closeDialog}><div className="modal-body"><p>This removes <strong>{workspace ? names.get(workspace.id) : 'the pipeline'}</strong> from your local workspace. Existing demo runs keep their own snapshots. Files on disk are unchanged.</p></div><div className="modal-footer"><button className="button" onClick={closeDialog}>Keep pipeline</button><button className="button danger" onClick={() => { setState(s => { const workspaces = s.workspaces.filter(w => w.id !== workspace?.id); return { ...s, workspaces, activeId: workspaces[0]?.id ?? '' }; }); closeDialog(); }}>Remove pipeline</button></div></Modal> : null}
    {dialog === 'import' ? <Modal title="Import a workflow" subtitle="Open an entrypoint YAML, a complete folder, or an exported ZIP package." onClose={closeDialog}><div className="import-options"><button onClick={() => fileInput.current?.click()}><Upload size={25} /><strong>Select files or ZIP</strong><span>Include the YAML and declared supporting files</span></button><button onClick={() => folderInput.current?.click()}><FolderOpen size={25} /><strong>Select package folder</strong><span>Only declared files are added to the workspace</span></button></div></Modal> : null}
    {dialog === 'entrypoint' ? <Modal title="Choose the entrypoint." subtitle="The package contains more than one pipeline document." onClose={closeDialog}><div className="modal-body"><label className="field">Entrypoint<select value={pickedEntry} onChange={e => setPickedEntry(e.target.value)}>{entries.map(path => <option key={path}>{path}</option>)}</select></label></div><div className="modal-footer"><span>All file paths remain relative to the package root.</span><button className="button primary" onClick={confirmEntry}>Open package<ArrowRight size={15} /></button></div></Modal> : null}
    <input hidden ref={fileInput} aria-label="Import pipeline package" type="file" multiple accept=".yaml,.yml,.zip,.txt,.json,.py,.md" onChange={e => { void importBrowser([...e.target.files ?? []]); e.target.value = ''; }} />
    <input hidden ref={folderInput} type="file" {...{ webkitdirectory: '', directory: '' }} onChange={e => { void importBrowser([...e.target.files ?? []]); e.target.value = ''; }} />
    {toast ? <div className="toast" role="status"><span>{toast}</span><button className="icon-button" aria-label="Dismiss notification" onClick={() => setToast('')}><X size={16} /></button></div> : null}
  </div></ThemeContext.Provider>;
}
