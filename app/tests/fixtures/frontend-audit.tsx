import { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { EngineInboxView } from '../../src/components/EngineInboxView';
import { EngineArtifactsView } from '../../src/components/EngineArtifactsView';
import SourceEditor from '../../src/components/SourceEditor';
import WorkspaceView from '../../src/components/WorkspaceView';
import { Modal } from '../../src/components/ui';
import { connectEngine } from '../../src/lib/engine/client';
import type { EngineController } from '../../src/lib/engine/useEngine';
import { validatePipeline } from '../../src/lib/validation';
import type { Workspace } from '../../src/lib/types';
import foreachSource from '../../../contracts/v1/fixtures/positive/foreach/pipeline.yaml?raw';
import '../../src/styles.css';

const engine: EngineController = {
  sessionKey: 'audit-fixture',
  info: {
    protocol: 'knotra.desktop/1',
    engineId: 'audit-engine',
    principalId: 'audit-user',
    version: 'fixture',
    capabilities: [],
  },
  connecting: false,
  syncing: false,
  error: '',
  runs: [],
  artifacts: [
    {
      id: 'artifact-1',
      name: 'hello.txt',
      mediaType: 'text/plain',
      size: 5,
      sha256: '2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824',
      origin: {},
    },
  ],
  profiles: [],
  resources: [],
  pending: [],
  requests: ['first', 'second'].map((name) => ({
    id: `request-${name}`,
    runId: `run-${name}`,
    instanceId: `root/${name}`,
    attemptId: 'attempt-1',
    status: 'open',
    prompt: `Review ${name} request.`,
    createdAt: '2026-10-01T00:00:00Z',
    deadline: '2030-01-01T00:00:00Z',
    inputs: { values: {}, artifacts: {} },
    responseSchema: { type: 'object' },
  })),
  connect: async () => {},
  disconnect: async () => {},
  refresh: async () => {},
  command: async <T,>(request: Parameters<EngineController['command']>[0]) => {
    if (request.op === 'upload') throw new Error('Upload failed in fixture.');
    return { accepted: true } as T;
  },
};

function Fixture() {
  const [open, setOpen] = useState(false);
  const [evidence, setEvidence] = useState('');
  const [inboxOpen, setInboxOpen] = useState(true);
  const [responseDrafts, setResponseDrafts] = useState<Record<string, string>>({});
  const [workspace, setWorkspace] = useState<Workspace>({
    id: 'audit-workspace',
    entrypoint: 'pipeline.yaml',
    source: foreachSource,
    savedSource: foreachSource,
    files: [],
    updatedAt: '2026-10-01T00:00:00Z',
  });
  switch (new URLSearchParams(location.search).get('component')) {
    case 'inbox':
      return inboxOpen ? (
        <EngineInboxView
          engine={engine}
          onRun={() => setInboxOpen(false)}
          responseDrafts={responseDrafts}
          onResponseDraftsChange={setResponseDrafts}
        />
      ) : (
        <button onClick={() => setInboxOpen(true)}>Return to inbox</button>
      );
    case 'artifacts':
      return <EngineArtifactsView engine={engine} />;
    case 'snapshot':
      return <SourceEditor source={'name: immutable-run\nvalue: original\n'} readOnly />;
    case 'workspace':
      return (
        <WorkspaceView
          workspace={workspace}
          validation={validatePipeline(workspace.source, [], workspace.entrypoint)}
          onChange={(source, files, positions) =>
            setWorkspace((current) => ({
              ...current,
              source,
              files: files ?? current.files,
              positions: positions ?? current.positions,
            }))
          }
          onSave={() => {}}
          onExport={() => {}}
          onRun={() => {}}
          onLibrary={() => {}}
          onAddNode={() => {}}
          onDelete={() => {}}
          onNotify={() => {}}
          demo={false}
          engineConnected={false}
          onEngineRun={() => {}}
        />
      );
    default:
      return (
        <>
          <button onClick={() => setOpen(true)}>Open resolution</button>
          {open ? (
            <Modal title="Resolve unknown outcome" onClose={() => setOpen(false)}>
              <label>
                Resolution evidence
                <textarea value={evidence} onChange={(event) => setEvidence(event.target.value)} />
              </label>
            </Modal>
          ) : null}
        </>
      );
  }
}

if (new URLSearchParams(location.search).get('component') === 'artifacts')
  await connectEngine('http://127.0.0.1:19879');
createRoot(document.getElementById('root')!).render(<Fixture />);
