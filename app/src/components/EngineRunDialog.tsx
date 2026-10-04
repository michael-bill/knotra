import { useI18n } from '../lib/i18n';
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
  const { t, locale, message } = useI18n();
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
  const [localError, setLocalError] = useState<{
    phrase: string;
    params?: Record<string, string | number>;
  }>();
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
    setLocalError(undefined);
    let localFailure: typeof localError;
    try {
      const values = JSON.parse(inputs);
      if (!values || typeof values !== 'object' || Array.isArray(values)) {
        localFailure = { phrase: 'Inputs must be a JSON object.' };
        throw new Error('Inputs must be a JSON object.');
      }
      const artifactBindings = { ...handles };
      for (const [name, port] of Object.entries(pipeline?.spec.inputs ?? {}))
        if (port.artifact?.collection) {
          const value = JSON.parse(artifactText[name] ?? '[]');
          if (!Array.isArray(value) || !value.every((id) => typeof id === 'string' && id.length)) {
            localFailure = {
              phrase: '{name} requires an array of registered artifact IDs.',
              params: { name },
            };
            throw new Error(`${name} requires an array of registered artifact IDs.`);
          }
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
      setLocalError(
        error instanceof SyntaxError
          ? { phrase: 'Invalid JSON: {message}', params: { message: caught.message } }
          : localFailure,
      );
      setDiagnostics(caught.diagnostics as EngineDiagnostic[]);
      if (start && frozen.current) {
        if (
          caught.code === 'input' ||
          (caught.status &&
            caught.status >= 400 &&
            caught.status < 500 &&
            ![408, 429].includes(caught.status))
        ) {
          // A definitive rejection has no pending command to reconcile.
          frozen.current = undefined;
          setBlocked(false);
        } else setBlocked(true);
      }
    } finally {
      setBusy(false);
    }
  }
  return (
    <Modal
      title={t('execution.runOnEngine')}
      subtitle={t('execution.theEngineChecksAdmissionAndFixesAnImmutable')}
      onClose={onClose}
    >
      <div className="modal-body engine-run-form">
        <label className="field">
          {t('execution.engineProfile')}
          <select
            aria-label={t('execution.engineProfile')}
            value={profile}
            onChange={(event) => setProfile(event.target.value)}
          >
            {!engine.profiles.length ? (
              <option value="">{t('execution.noProfilesAvailable')}</option>
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
          {t('execution.workflowValuesJson')}
          <textarea
            aria-label={t('execution.workflowInputValues')}
            className="code-input"
            rows={7}
            value={inputs}
            onChange={(event) => setInputs(event.target.value)}
          />
        </label>
        <p className="small muted">
          {t('execution.inputPortsPortsOmittedDefaultsAreSuppliedBy', {
            ports:
              Object.entries(pipeline?.spec.inputs ?? {})
                .filter(([, port]) => !port.artifact)
                .map(([name, port]) =>
                  port.required ? t('execution.nameRequired', { name }) : name,
                )
                .join(', ') || t('execution.none'),
          })}
        </p>
        {Object.entries(pipeline?.spec.inputs ?? {})
          .filter(([, port]) => port.artifact)
          .map(([name, port]) => (
            <label className="field" key={name}>
              {t('execution.nameInputArtifact', { name })}
              {port.artifact?.collection ? (
                <textarea
                  aria-label={t('execution.artifactHandlesForName', { name })}
                  placeholder='["artifact-id"]'
                  value={artifactText[name] ?? '[]'}
                  onChange={(event) =>
                    setArtifactText((current) => ({ ...current, [name]: event.target.value }))
                  }
                />
              ) : (
                <select
                  aria-label={t('execution.artifactHandleForName', { name })}
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
                  <option value="">{t('execution.chooseARegisteredArtifact')}</option>
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
                        {artifact.name} · {size(artifact.size, locale)}
                      </option>
                    ))}
                </select>
              )}
            </label>
          ))}
        {error ? (
          <p className="form-error" role="alert">
            {localError ? message(localError.phrase, localError.params) : error}
          </p>
        ) : null}
        {blocked ? (
          <div className="notice small">{t('execution.thisCommandHasASavedOperationIdClose')}</div>
        ) : null}
        <Diagnostics diagnostics={diagnostics} />
        {validated ? (
          <p className="small">
            {t('execution.engineAdmissionChecksPassedStartChecksAgainBefore')}
          </p>
        ) : null}
      </div>
      <div className="modal-footer">
        <button
          className="button"
          disabled={busy || !profile || blocked}
          onClick={() => void check(false)}
        >
          {t('execution.checkWithEngine')}
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
          {busy ? t('execution.preparing') : t('execution.startRun')}
        </button>
      </div>
    </Modal>
  );
}
