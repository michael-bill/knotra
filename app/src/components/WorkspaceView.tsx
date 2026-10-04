import { editorLayerKeys } from '../lib/editorLabels';
import { useI18n } from '../lib/i18n';
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
  const { t, locale, message } = useI18n();
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
  const [requestedScope, setScope] = useState<string[]>([]);
  const [diagnosticsOpen, setDiagnosticsOpen] = useState(false);
  const [selectedFile, setSelectedFile] = useState<string>();
  const [removeNode, setRemoveNode] = useState(false);
  const upload = useRef<HTMLInputElement>(null);
  const pipeline = validation.pipeline;
  const { graph, scope } = useMemo(() => {
    let graph = pipeline?.spec;
    const scope: string[] = [];
    for (const id of requestedScope) {
      const node = graph?.nodes[id];
      if (!node || !['foreach', 'loop'].includes(node.type)) break;
      scope.push(id);
      graph = record(record(node[node.type]).body) as unknown as Graph;
    }
    return { graph, scope };
  }, [pipeline, requestedScope]);
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
        t('editor.changeNotSavedMessage', {
          message: message(
            check.diagnostics.find((d) => d.severity === 'error')?.message ??
              t('editor.invalidBlockConfiguration'),
          ),
        }),
      );
      return;
    }
    onChange(source);
  }
  function connect(connection: PortConnection) {
    if (!graph) return;
    const error = connectionError(graph, connection, importedGraphs);
    if (error) {
      onNotify(message(error));
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
      t('editor.sourceConnectedToTarget', {
        source: `${connection.source}.${connection.sourcePort}`,
        target: `${connection.target}.${connection.targetPort}`,
      }),
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
        title={t('editor.makeRoomForYourNextIdea')}
        action={
          <button className="button primary" onClick={onLibrary}>
            <Plus size={16} />
            {t('editor.createPipeline')}
          </button>
        }
      >
        {t('editor.startFromAContractExampleOrOpenAn')}
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
            <span>
              {pipeline?.metadata.title ?? pipeline?.metadata.name ?? t('editor.pipelineDraft')}
            </span>
          </h1>
          {workspace.source !== workspace.savedSource ? (
            <span
              className="unsaved-dot title-unsaved"
              title={t('editor.unsavedDraft')}
              aria-label={t('editor.unsavedDraft')}
            />
          ) : null}
          <span className="heading-version">
            {pipeline?.metadata.version ? `v${pipeline.metadata.version}` : t('editor.draft')}
          </span>
        </div>
        <div className="heading-actions">
          <button
            className="button small-button"
            onClick={onSave}
            title={t('editor.saveLocalDraftCtrlS')}
          >
            <Save size={14} />
            {t('common.save')}
          </button>
          <button className="button small-button" onClick={onExport}>
            <Download size={14} />
            {t('common.export')}
          </button>
          <button
            className="button small-button primary"
            onClick={onEngineRun}
            disabled={errors.length > 0}
            title={t(
              engineConnected
                ? 'editor.checkAndStartOnYourEngine'
                : 'editor.connectAnEngineInSettings',
            )}
          >
            <Play size={14} />
            {t(engineConnected ? 'common.run' : 'resources.connectEngine')}
          </button>
          {demo || pipeline?.metadata.name === 'research-brief' ? (
            <button
              className="button small-button primary"
              onClick={onRun}
              disabled={!demo || errors.length > 0}
              title={t(
                demo
                  ? 'editor.runTheGuidedDemoWithSampleOutputs'
                  : 'editor.theUnchangedResearchBriefTemplateOffersAGuided',
              )}
            >
              <Play size={14} fill="currentColor" />
              {t('editor.runDemo')}
            </button>
          ) : null}
        </div>
      </header>
      <div className="workspace-toolbar">
        <div className="segmented" role="tablist" aria-label={t('editor.pipelineView')}>
          <button
            role="tab"
            aria-selected={view === 'graph'}
            className={view === 'graph' ? 'active' : ''}
            onClick={() => setView('graph')}
          >
            <Workflow size={15} />
            {t('editor.canvas')}
          </button>
          <button
            role="tab"
            aria-selected={view === 'source'}
            className={view === 'source' ? 'active' : ''}
            onClick={() => setView('source')}
          >
            <Braces size={15} />
            {t('editor.code')}
          </button>
          <button
            role="tab"
            aria-selected={view === 'files'}
            className={view === 'files' ? 'active' : ''}
            onClick={() => setView('files')}
          >
            <FolderOpen size={15} />
            {t('editor.files')}
            <span className="counter">{workspace.files.length + 1}</span>
          </button>
        </div>
        <div className="inline">
          <span className="editor-guidance">{t('editor.dragAnOutputToAnInputSelectA')}</span>
          <button
            className="button small-button"
            onClick={() => {
              setView('graph');
              setSelected('');
            }}
            disabled={!pipeline}
          >
            <Settings2 size={14} />
            {t('editor.workflow')}
          </button>
          <span className="small muted node-count">
            {t('editor.countNodes', {
              count: pipeline ? Object.keys(pipeline.spec.nodes).length : '—',
            })}
          </span>
          <button
            className="button small-button"
            onClick={() => onAddNode(scope)}
            disabled={!pipeline}
          >
            <Plus size={15} />
            {t('editor.addNode')}
          </button>
          <button
            className="icon-button"
            onClick={onDelete}
            aria-label={t('editor.deletePipeline')}
            title={t('editor.deletePipeline')}
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
            {t('editor.parentGraph')}
          </button>
          <span>
            {scope.join(' / ')}
            <ChevronRight size={12} />
            {t('editor.body')}
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
              title={t('editor.theYamlNeedsALittleAttention')}
              action={
                <button className="button" onClick={() => setView('source')}>
                  {t('editor.openYamlEditor')}
                </button>
              }
            >
              {t('editor.fixTheParseOrStructuralErrorsBelowTo')}
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
              {t('editor.countLines', { count: workspace.source.split('\n').length })}
              <span>{t('editor.yamlIsTheSourceOfTruthChangesUpdate')}</span>
            </div>
          </div>
        ) : (
          <div className="files-view">
            <div className="file-list">
              <div className="file-list-title">
                {t('editor.packageFiles')}
                <button
                  className="icon-button"
                  onClick={() => upload.current?.click()}
                  aria-label={t('editor.addPackageFile')}
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
                <small>{t('editor.entry')}</small>
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
                {t('editor.onlyFilesDeclaredIn')} <code>spec.files</code>{' '}
                {t('editor.belongToTheExportedPackage')}
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
                      throw new Error(t('editor.aFileWithThisNameAlreadyExists'));
                    if (pipeline) {
                      const declared = pipeline.spec.files ?? [];
                      const additions = files
                        .map((f) => f.path)
                        .filter((p) => !declared.includes(p));
                      let source = workspace.source;
                      if (additions.length) {
                        const doc = parseDocument(source);
                        doc.setIn(['spec', 'files'], [...declared, ...additions]);
                        source = doc.toString();
                      }
                      onChange(source, [...workspace.files, ...files]);
                    } else onNotify(t('editor.fixThePipelineStructureBeforeAddingFiles'));
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
                    <span>{size(unbase64(file.content).length, locale)} · UTF-8</span>
                  </div>
                  <SourceEditor
                    key={file.path}
                    source={text}
                    label="editor.packageFileContents"
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
                <Empty icon={<File />} title={selectedFile ?? t('editor.selectAFile')}>
                  {file
                    ? `${size(unbase64(file.content).length, locale)} · ${t('editor.binaryFilePreservedWithThePackage')}`
                    : t('editor.chooseAPackageFileToInspectItsContents')}
                </Empty>
              )}
            </div>
          </div>
        )}
      </div>
      {removeNode ? (
        <div className="inline-confirm">
          <span>
            {t('editor.remove')} <strong>{selected}</strong>
            {t('editor.existingReferencesWillNeedToBeUpdatedIn')}
          </span>
          <button
            className="button danger small-button"
            onClick={() => {
              mutate((doc, path) => doc.deleteIn([...path, 'nodes', selected]));
              setRemoveNode(false);
              setSelected('');
            }}
          >
            {t('editor.removeNode')}
          </button>
          <button
            className="icon-button"
            aria-label={t('editor.keepNode')}
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
              ? t(
                  errors.length === 1
                    ? 'editor.countIssueToResolve'
                    : 'editor.countIssuesToResolve',
                  {
                    count: errors.length,
                  },
                )
              : t('editor.localChecksPassed')}
          </strong>
          <span>{t('editor.engineChecksPending')}</span>
          <ChevronRight size={14} className={diagnosticsOpen ? 'rotated' : ''} />
          <small>{t(diagnosticsOpen ? 'editor.hideDiagnostics' : 'editor.showDiagnostics')}</small>
        </button>
        {diagnosticsOpen ? (
          <div className="diagnostics" role="log">
            {validation.diagnostics.map((d, i) => (
              <div className={`diagnostic diagnostic-${d.severity}`} key={i}>
                <span>{d.severity === 'error' ? <AlertTriangle size={14} /> : <ShieldIcon />}</span>
                <div>
                  <strong>{d.code}</strong>
                  <p>{message(d.message)}</p>
                  <code>
                    {d.path}
                    {d.line ? `:${d.line}` : ''} · {t(editorLayerKeys[d.layer])}
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
