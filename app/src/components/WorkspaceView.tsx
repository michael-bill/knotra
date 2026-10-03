import { useRef, useState, useMemo, useEffect } from 'react';
import { parseDocument } from 'yaml';
import {
  Workflow,
  Braces,
  FolderOpen,
  Plus,
  ChevronRight,
  ArrowLeft,
  FileText,
  CheckCircle2,
  AlertTriangle,
  X,
  Upload,
  Save,
  Download,
  Play,
  MoreHorizontal,
  Trash2,
  File,
  Sparkles,
  Settings2,
} from 'lucide-react';
import GraphView from './GraphView';
import SourceEditor from './SourceEditor';
import Inspector from './Inspector';
import WorkflowSetup from './WorkflowSetup';
import { Empty, size } from './ui';
import { base64, decodeText, textBytes, unbase64 } from '../lib/bytes';
import { connectionError, type PortConnection } from '../lib/connections';
import { parsePipeline, validatePipeline } from '../lib/validation';
import { record, type Graph, type Validation, type Workspace } from '../lib/types';

export default function WorkspaceView({
  workspace,
  validation,
  onChange,
  onSave,
  onExport,
  onRun,
  onLibrary,
  onAddNode,
  onDelete,
  onNotify,
  demo,
  engineConnected,
  onEngineRun,
  resourcesRequested,
}: {
  engineConnected?: boolean;
  onEngineRun: () => void;
  resourcesRequested?: 'models' | 'mcp' | 'sandboxes' | 'secrets';
  workspace?: Workspace;
  validation: Validation;
  onChange: (
    source: string,
    files?: Workspace['files'],
    positions?: Workspace['positions'],
  ) => void;
  onSave: () => void;
  onExport: () => void;
  onRun: () => void;
  onLibrary: () => void;
  onAddNode: (scope: string[]) => void;
  onDelete: () => void;
  onNotify: (message: string) => void;
  demo: boolean;
}) {
  const [showPorts, setShowPorts] = useState(false);
  const [view, setView] = useState<'graph' | 'source' | 'files'>(
    resourcesRequested === 'secrets' ? 'source' : 'graph',
  );
  const [selected, setSelected] = useState<string>(() =>
    resourcesRequested
      ? ''
      : validation.pipeline?.spec.nodes.research
        ? 'research'
        : (Object.keys(validation.pipeline?.spec.nodes ?? {})[0] ?? ''),
  );
  const [scope, setScope] = useState<string[]>([]);
  const [diagnosticsOpen, setDiagnosticsOpen] = useState(false);
  const [selectedFile, setSelectedFile] = useState<string>();
  const [removeNode, setRemoveNode] = useState(false);
  const upload = useRef<HTMLInputElement>(null);
  const pipeline = validation.pipeline;
  const graph = useMemo(() => {
    let graph = pipeline?.spec;
    for (const id of scope) {
      const node = graph?.nodes[id];
      if (!node) return pipeline?.spec;
      graph = record(record(node[node.type]).body) as unknown as Graph;
    }
    return graph;
  }, [pipeline, scope]);
  const importedGraphs = useMemo(() => {
    const graphs = new Map<string, Graph>();
    for (const file of workspace?.files ?? []) {
      if (!/\.ya?ml$/.test(file.path)) continue;
      try {
        const pipeline = parsePipeline(decodeText(file.content));
        if (pipeline) graphs.set(file.path, pipeline.spec);
      } catch {
        /* Validation reports unreadable imports. */
      }
    }
    return graphs;
  }, [workspace?.files]);
  const node = selected ? graph?.nodes[selected] : undefined;
  const knownNodes = useRef(new Set(Object.keys(graph?.nodes ?? {})));
  useEffect(() => {
    if (!graph) return;
    const added = Object.keys(graph.nodes).find((id) => !knownNodes.current.has(id));
    knownNodes.current = new Set(Object.keys(graph.nodes));
    if (added) {
      setSelected(added);
      setShowPorts(false);
    } else if (selected && !graph.nodes[selected]) setSelected(Object.keys(graph.nodes)[0] ?? '');
  }, [graph, selected]);
  const errors = validation.diagnostics.filter((d) => d.severity === 'error');
  function mutate(action: (document: ReturnType<typeof parseDocument>, path: string[]) => void) {
    if (!workspace || !pipeline) return;
    const document = parseDocument(workspace.source);
    const path = ['spec'];
    let g: Graph = pipeline.spec;
    for (const id of scope) {
      path.push('nodes', id, g.nodes[id].type, 'body');
      g = record(record(g.nodes[id][g.nodes[id].type]).body) as unknown as Graph;
    }
    action(document, path);
    const source = document.toString({ lineWidth: 110 });
    const check = validatePipeline(source, workspace.files, workspace.entrypoint);
    if (!check.pipeline) {
      onNotify(
        `Change not saved: ${check.diagnostics.find((d) => d.severity === 'error')?.message ?? 'invalid block configuration'}`,
      );
      return;
    }
    onChange(source);
  }
  function connect(connection: PortConnection) {
    if (!graph) return;
    const error = connectionError(graph, connection, importedGraphs);
    if (error) {
      onNotify(error);
      return;
    }
    mutate((doc, path) =>
      doc.setIn([...path, 'nodes', connection.target, 'inputs', connection.targetPort, 'bind'], {
        from: `nodes.${connection.source}.outputs.${connection.sourcePort}`,
      }),
    );
    setSelected(connection.target);
    setShowPorts(true);
    onNotify(
      `${connection.source}.${connection.sourcePort} connected to ${connection.target}.${connection.targetPort}.`,
    );
  }
  function openBody(id: string) {
    const n = graph?.nodes[id];
    if (n && ['foreach', 'loop'].includes(n.type)) {
      setScope([...scope, id]);
      const child = record(record(n[n.type]).body) as unknown as Graph;
      setSelected(Object.keys(child.nodes)[0]);
    }
  }
  if (!workspace)
    return (
      <Empty
        icon={<Workflow />}
        title="Make room for your next idea"
        action={
          <button className="button primary" onClick={onLibrary}>
            <Plus size={16} />
            Create pipeline
          </button>
        }
      >
        Start from a contract example or open an existing package.
      </Empty>
    );
  const file = workspace.files.find((f) => f.path === selectedFile);
  let text: string | undefined;
  if (file)
    try {
      text = decodeText(file.content);
    } catch {
      /* Binary file: show metadata. */
    }
  return (
    <div className="workspace-view">
      <header className="workspace-header">
        <div className="workspace-title">
          <h1 title={pipeline?.metadata.description}>
            <span>{pipeline?.metadata.title ?? pipeline?.metadata.name ?? 'Pipeline draft'}</span>
          </h1>
          {workspace.source !== workspace.savedSource ? (
            <span
              className="unsaved-dot title-unsaved"
              title="Unsaved draft"
              aria-label="Unsaved draft"
            />
          ) : null}
          <span className="heading-version">
            {pipeline?.metadata.version ? `v${pipeline.metadata.version}` : 'Draft'}
          </span>
        </div>
        <div className="heading-actions">
          <button
            className="button small-button"
            onClick={onSave}
            title="Save local draft (⌘/Ctrl+S)"
          >
            <Save size={14} />
            Save
          </button>
          <button className="button small-button" onClick={onExport}>
            <Download size={14} />
            Export
          </button>
          <button
            className="button small-button primary"
            onClick={onEngineRun}
            disabled={!engineConnected || errors.length > 0}
            title={
              engineConnected ? 'Check and start on your engine' : 'Connect an engine in Settings'
            }
          >
            <Play size={14} />
            Run
          </button>
          <button
            className="button small-button primary"
            onClick={onRun}
            disabled={!demo || errors.length > 0}
            title={
              demo
                ? 'Run the guided demo with sample outputs'
                : 'The unchanged Research brief template offers a guided demo.'
            }
          >
            <Play size={14} fill="currentColor" />
            Run demo
          </button>
        </div>
      </header>
      <div className="workspace-toolbar">
        <div className="segmented" role="tablist" aria-label="Pipeline view">
          <button
            role="tab"
            aria-selected={view === 'graph'}
            className={view === 'graph' ? 'active' : ''}
            onClick={() => setView('graph')}
          >
            <Workflow size={15} />
            Canvas
          </button>
          <button
            role="tab"
            aria-selected={view === 'source'}
            className={view === 'source' ? 'active' : ''}
            onClick={() => setView('source')}
          >
            <Braces size={15} />
            Code
          </button>
          <button
            role="tab"
            aria-selected={view === 'files'}
            className={view === 'files' ? 'active' : ''}
            onClick={() => setView('files')}
          >
            <FolderOpen size={15} />
            Files<span className="counter">{workspace.files.length + 1}</span>
          </button>
        </div>
        <div className="inline">
          <span className="editor-guidance">
            Drag an output to an input · Select a block to set it up
          </span>
          <button
            className="button small-button"
            onClick={() => {
              setView('graph');
              setSelected('');
            }}
            disabled={!pipeline}
          >
            <Settings2 size={14} />
            Workflow
          </button>
          <span className="small muted node-count">
            {pipeline ? Object.keys(pipeline.spec.nodes).length : '—'} nodes
          </span>
          <button
            className="button small-button"
            onClick={() => onAddNode(scope)}
            disabled={!pipeline}
          >
            <Plus size={15} />
            Add node
          </button>
          <button
            className="icon-button"
            onClick={onDelete}
            aria-label="Delete pipeline"
            title="Delete pipeline"
          >
            <Trash2 size={15} />
          </button>
        </div>
      </div>
      {scope.length ? (
        <div className="scope-bar">
          <button
            className="text-button"
            onClick={() => {
              setScope(scope.slice(0, -1));
              setSelected(scope.at(-1) ?? '');
            }}
          >
            <ArrowLeft size={14} />
            Parent graph
          </button>
          <span>
            {scope.join(' / ')}
            <ChevronRight size={12} />
            body
          </span>
        </div>
      ) : null}
      <div className="workspace-body">
        {view === 'graph' ? (
          graph ? (
            <>
              <GraphView
                key={workspace.id + scope.join('/')}
                graph={graph}
                positions={Object.fromEntries(
                  Object.entries(workspace.positions ?? {})
                    .filter(([key]) => key.startsWith(scope.join('/') + '::'))
                    .map(([key, value]) => [key.split('::')[1], value]),
                )}
                onLayout={(positions) =>
                  onChange(workspace.source, undefined, {
                    ...workspace.positions,
                    ...Object.fromEntries(
                      Object.entries(positions).map(([id, position]) => [
                        scope.join('/') + '::' + id,
                        position,
                      ]),
                    ),
                  })
                }
                onMove={(id, position) =>
                  onChange(workspace.source, undefined, {
                    ...workspace.positions,
                    [scope.join('/') + '::' + id]: position,
                  })
                }
                importedGraphs={importedGraphs}
                selected={selected}
                onSelect={(id) => {
                  setSelected(id);
                  setShowPorts(false);
                  setRemoveNode(false);
                }}
                onOpenBody={openBody}
                onConnect={connect}
                onNotify={onNotify}
              />
              {node ? (
                <Inspector
                  showPorts={showPorts}
                  onWorkflow={() => setSelected('')}
                  key={selected + scope.join('/')}
                  resources={pipeline!.spec}
                  files={workspace.files.map((file) => file.path)}
                  onNotify={onNotify}
                  id={selected}
                  node={node}
                  graph={graph}
                  importedGraphs={importedGraphs}
                  onEdit={(key, value) =>
                    mutate((doc, path) => {
                      if (value === undefined) doc.deleteIn([...path, 'nodes', selected, key]);
                      else doc.setIn([...path, 'nodes', selected, key], value);
                    })
                  }
                  onSource={() => setView('source')}
                  onDuplicate={() =>
                    mutate((doc, path) => {
                      let id = `${selected}_copy`;
                      let i = 2;
                      while (graph.nodes[id]) id = `${selected}_copy${i++}`;
                      doc.setIn([...path, 'nodes', id], node);
                      setSelected(id);
                    })
                  }
                  onRemove={() => setRemoveNode(true)}
                  onBody={() => openBody(selected)}
                />
              ) : (
                <WorkflowSetup
                  showResources={!!resourcesRequested && resourcesRequested !== 'secrets'}
                  pipeline={pipeline!}
                  graph={graph}
                  imports={importedGraphs}
                  scope={scope}
                  onGraphEdit={(key, value) =>
                    mutate((doc, path) => doc.setIn([...path, key], value))
                  }
                  onMetadata={(title) => mutate((doc) => doc.setIn(['metadata', 'title'], title))}
                  onResources={(kind, value) => mutate((doc) => doc.setIn(['spec', kind], value))}
                />
              )}
            </>
          ) : (
            <Empty
              icon={<Braces />}
              title="The YAML needs a little attention"
              action={
                <button className="button" onClick={() => setView('source')}>
                  Open YAML editor
                </button>
              }
            >
              Fix the parse or structural errors below to see the graph.
            </Empty>
          )
        ) : view === 'source' ? (
          <div className="editor-wrap">
            <div className="editor-file">
              <FileText size={14} />
              {workspace.entrypoint}
              <span>YAML 1.2 · UTF-8</span>
            </div>
            <SourceEditor source={workspace.source} onChange={onChange} />
            <div className="editor-status">
              {workspace.source.split('\n').length} lines
              <span>YAML is the source of truth. Changes update the graph.</span>
            </div>
          </div>
        ) : (
          <div className="files-view">
            <div className="file-list">
              <div className="file-list-title">
                PACKAGE FILES
                <button
                  className="icon-button"
                  onClick={() => upload.current?.click()}
                  aria-label="Add package file"
                >
                  <Plus size={16} />
                </button>
              </div>
              <button
                className={!selectedFile ? 'active' : ''}
                onClick={() => setSelectedFile(undefined)}
              >
                <FileText size={16} />
                <span>{workspace.entrypoint}</span>
                <small>entry</small>
              </button>
              {workspace.files.map((f) => (
                <button
                  className={selectedFile === f.path ? 'active' : ''}
                  key={f.path}
                  onClick={() => setSelectedFile(f.path)}
                >
                  <File size={15} />
                  <span>{f.path}</span>
                </button>
              ))}
              <p>
                Only files declared in <code>spec.files</code> belong to the exported package.
              </p>
              <input
                ref={upload}
                hidden
                type="file"
                multiple
                onChange={async (e) => {
                  try {
                    const files = await Promise.all(
                      [...(e.target.files ?? [])].map(async (f) => ({
                        path: f.name,
                        content: base64(new Uint8Array(await f.arrayBuffer())),
                      })),
                    );
                    if (
                      files.some(
                        (f) =>
                          workspace.files.some(
                            (existing) => existing.path.toLowerCase() === f.path.toLowerCase(),
                          ) || f.path === workspace.entrypoint,
                      )
                    )
                      throw new Error('A file with this name already exists.');
                    if (pipeline) {
                      const doc = parseDocument(workspace.source);
                      doc.setIn(
                        ['spec', 'files'],
                        [...(pipeline.spec.files ?? []), ...files.map((f) => f.path)],
                      );
                      onChange(doc.toString(), [...workspace.files, ...files]);
                    } else onNotify('Fix the pipeline structure before adding files.');
                  } catch (error) {
                    onNotify(String(error));
                  }
                  e.target.value = '';
                }}
              />
            </div>
            <div className="file-preview">
              {!selectedFile ? (
                <SourceEditor source={workspace.source} onChange={onChange} />
              ) : file && text !== undefined ? (
                <>
                  <div className="editor-file">
                    <FileText size={14} />
                    {file.path}
                    <span>{size(unbase64(file.content).length)} · UTF-8</span>
                  </div>
                  <SourceEditor
                    key={file.path}
                    source={text}
                    label="Package file contents"
                    onChange={(value) =>
                      onChange(
                        workspace.source,
                        workspace.files.map((f) =>
                          f.path === file.path ? { ...f, content: base64(textBytes(value)) } : f,
                        ),
                      )
                    }
                  />
                </>
              ) : (
                <Empty icon={<File />} title={selectedFile ?? 'Select a file'}>
                  {file
                    ? `${size(unbase64(file.content).length)} · Binary file preserved with the package`
                    : 'Choose a package file to inspect its contents.'}
                </Empty>
              )}
            </div>
          </div>
        )}
      </div>
      {removeNode ? (
        <div className="inline-confirm">
          <span>
            Remove <strong>{selected}</strong>? Existing references will need to be updated in YAML.
          </span>
          <button
            className="button danger small-button"
            onClick={() => {
              mutate((doc, path) => doc.deleteIn([...path, 'nodes', selected]));
              setRemoveNode(false);
              setSelected('');
            }}
          >
            Remove node
          </button>
          <button
            className="icon-button"
            aria-label="Keep node"
            onClick={() => setRemoveNode(false)}
          >
            <X size={17} />
          </button>
        </div>
      ) : null}
      <div className="validation-panel">
        <button
          className={`validation-summary ${errors.length ? 'has-errors' : ''}`}
          onClick={() => setDiagnosticsOpen(!diagnosticsOpen)}
          aria-expanded={diagnosticsOpen}
        >
          {errors.length ? <AlertTriangle size={16} /> : <CheckCircle2 size={16} />}
          <strong>
            {errors.length
              ? `${errors.length} ${errors.length === 1 ? 'issue' : 'issues'} to resolve`
              : 'Local checks passed'}
          </strong>
          <span>Engine checks pending</span>
          <ChevronRight size={14} className={diagnosticsOpen ? 'rotated' : ''} />
          <small>{diagnosticsOpen ? 'Hide' : 'Show'} diagnostics</small>
        </button>
        {diagnosticsOpen ? (
          <div className="diagnostics" role="log">
            {validation.diagnostics.map((d, i) => (
              <div className={`diagnostic diagnostic-${d.severity}`} key={i}>
                <span>{d.severity === 'error' ? <AlertTriangle size={14} /> : <ShieldIcon />}</span>
                <div>
                  <strong>{d.code}</strong>
                  <p>{d.message}</p>
                  <code>
                    {d.path}
                    {d.line ? `:${d.line}` : ''} · {d.layer}
                  </code>
                </div>
              </div>
            ))}
          </div>
        ) : null}
      </div>
    </div>
  );
}

function ShieldIcon() {
  return <span className="dot" />;
}
