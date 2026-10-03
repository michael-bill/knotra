import { useEffect, useRef, useState } from 'react';
import { Play } from 'lucide-react';
import type { Workspace } from '../lib/types';
import { parsePipeline } from '../lib/validation';
import { engineError } from '../lib/engine/client';
import { enginePackage } from '../lib/engine/package';
import type { EngineDiagnostic, EngineRun } from '../lib/engine/types';
import type { EngineController } from '../lib/engine/useEngine';
import { Modal, size } from './ui';
import { Diagnostics } from './EngineDiagnostics';

export function EngineRunDialog({
  workspace,
  engine,
  onClose,
  onStarted,
}: {
  workspace: Workspace;
  engine: EngineController;
  onClose: () => void;
  onStarted: (id: string) => void;
}) {
  const pipeline = parsePipeline(workspace.source);
  const [profile, setProfile] = useState(engine.profiles[0]?.id ?? '');
  const [inputs, setInputs] = useState(() =>
    JSON.stringify(
      Object.fromEntries(
        Object.entries(pipeline?.spec.inputs ?? {})
          .filter(([, port]) => !port.artifact && 'default' in port)
          .map(([name, port]) => [name, port.default]),
      ),
      null,
      2,
    ),
  );
  const [handles, setHandles] = useState<Record<string, string | string[]>>({});
  const [artifactText, setArtifactText] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [diagnostics, setDiagnostics] = useState<EngineDiagnostic[]>([]);
  const [validated, setValidated] = useState(false);
  const [blocked, setBlocked] = useState(false);
  const frozen = useRef<{ definitionId?: string; startId: string; publishId: string } | undefined>(
    undefined,
  );
  useEffect(() => {
    setValidated(false);
  }, [profile, inputs, handles, artifactText]);
  async function check(start: boolean) {
    setBusy(true);
    setError('');
    try {
      const values = JSON.parse(inputs);
      if (!values || typeof values !== 'object' || Array.isArray(values))
        throw new Error('Inputs must be a JSON object.');
      const artifactBindings = { ...handles };
      for (const [name, port] of Object.entries(pipeline?.spec.inputs ?? {}))
        if (port.artifact?.collection) {
          const value = JSON.parse(artifactText[name] ?? '[]');
          if (!Array.isArray(value) || !value.every((id) => typeof id === 'string' && id.length))
            throw new Error(`${name} requires an array of registered artifact IDs.`);
          artifactBindings[name] = value;
        }
      const pack = enginePackage(workspace);
      const admission = await engine.command<{ valid: boolean; diagnostics: EngineDiagnostic[] }>({
        op: 'validate',
        package: pack,
        profile,
        inputs: values,
        artifacts: artifactBindings,
      });
      setDiagnostics(admission.diagnostics);
      setValidated(admission.valid);
      if (!admission.valid || !start) return;
      frozen.current ??= { startId: crypto.randomUUID(), publishId: crypto.randomUUID() };
      if (!frozen.current.definitionId) {
        const { definition } = await engine.command<{ definition: { id: string } }>({
          op: 'publish',
          package: pack,
          operationId: frozen.current.publishId,
        });
        frozen.current.definitionId = definition.id;
      }
      const { run } = await engine.command<{ run: EngineRun }>({
        op: 'start',
        definitionId: frozen.current.definitionId!,
        profile,
        inputs: values,
        artifacts: artifactBindings,
        operationId: frozen.current.startId,
      });
      onStarted(run.id);
      onClose();
    } catch (error) {
      const caught = engineError(error);
      setError(caught.message);
      setDiagnostics(caught.diagnostics as EngineDiagnostic[]);
      if (start && frozen.current) setBlocked(true);
    } finally {
      setBusy(false);
    }
  }
  return (
    <Modal
      title="Run on engine"
      subtitle="The engine checks admission and fixes an immutable execution plan."
      onClose={onClose}
    >
      <div className="modal-body engine-run-form">
        <label className="field">
          Engine profile
          <select
            aria-label="Engine profile"
            value={profile}
            onChange={(event) => setProfile(event.target.value)}
          >
            {!engine.profiles.length ? (
              <option value="">No profiles available</option>
            ) : (
              engine.profiles.map((profile) => (
                <option key={profile.id} value={profile.id}>
                  {profile.title} · {profile.revision}
                </option>
              ))
            )}
          </select>
        </label>
        <label className="field">
          Workflow values (JSON)
          <textarea
            aria-label="Workflow input values"
            className="code-input"
            rows={7}
            value={inputs}
            onChange={(event) => setInputs(event.target.value)}
          />
        </label>
        <p className="small muted">
          Input ports:{' '}
          {Object.entries(pipeline?.spec.inputs ?? {})
            .filter(([, port]) => !port.artifact)
            .map(([name, port]) => `${name}${port.required ? ' (required)' : ''}`)
            .join(', ') || 'none'}
          . Omitted defaults are supplied by the engine.
        </p>
        {Object.entries(pipeline?.spec.inputs ?? {})
          .filter(([, port]) => port.artifact)
          .map(([name, port]) => (
            <label className="field" key={name}>
              {name} · input artifact
              {port.artifact?.collection ? (
                <textarea
                  aria-label={`Artifact handles for ${name}`}
                  placeholder='["artifact-id"]'
                  value={artifactText[name] ?? '[]'}
                  onChange={(event) =>
                    setArtifactText((current) => ({ ...current, [name]: event.target.value }))
                  }
                />
              ) : (
                <select
                  aria-label={`Artifact handle for ${name}`}
                  value={String(handles[name] ?? '')}
                  onChange={(event) =>
                    setHandles((current) => {
                      const next = { ...current };
                      if (event.target.value) next[name] = event.target.value;
                      else delete next[name];
                      return next;
                    })
                  }
                >
                  <option value="">Choose a registered artifact</option>
                  {engine.artifacts
                    .filter((artifact) =>
                      port.artifact?.mediaTypes.some(
                        (type) =>
                          type === artifact.mediaType ||
                          type === '*/*' ||
                          (type.endsWith('/*') && artifact.mediaType.startsWith(type.slice(0, -1))),
                      ),
                    )
                    .map((artifact) => (
                      <option key={artifact.id} value={artifact.id}>
                        {artifact.name} · {size(artifact.size)}
                      </option>
                    ))}
                </select>
              )}
            </label>
          ))}
        {error ? (
          <p className="form-error" role="alert">
            {error}
          </p>
        ) : null}
        {blocked ? (
          <div className="notice small">
            This command has a saved operation ID. Close this dialog and reconcile it from the
            engine banner before submitting a replacement.
          </div>
        ) : null}
        <Diagnostics diagnostics={diagnostics} />
        {validated ? (
          <p className="small">
            Engine admission checks passed. Start checks again before accepting the plan.
          </p>
        ) : null}
      </div>
      <div className="modal-footer">
        <button
          className="button"
          disabled={busy || !profile || blocked}
          onClick={() => void check(false)}
        >
          Check with engine
        </button>
        <button
          className="button primary"
          disabled={
            busy ||
            !profile ||
            blocked ||
            engine.pending.some((item) => ['publish', 'start'].includes(item.request.op))
          }
          onClick={() => void check(true)}
        >
          <Play size={15} />
          {busy ? 'Preparing…' : 'Start run'}
        </button>
      </div>
    </Modal>
  );
}
