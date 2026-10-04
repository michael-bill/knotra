import { useI18n } from '../lib/i18n';
import { useEffect, useState } from 'react';
import { Download, Search, FileBox } from 'lucide-react';
import { base64, decodeText } from '../lib/bytes';
import { engineError, exportEngineArtifact, downloadArtifact } from '../lib/engine/client';
import type { EngineController } from '../lib/engine/useEngine';
import { Empty, size } from './ui';

export function EngineArtifactsView({ engine }: { engine: EngineController }) {
  const { t, locale, message } = useI18n();
  const [selected, setSelected] = useState('');
  const [query, setQuery] = useState('');
  const [preview, setPreview] = useState('');
  const [previewNotice, setPreviewNotice] = useState('');
  const [error, setError] = useState('');
  const [localError, setLocalError] = useState(false);
  const [busy, setBusy] = useState(false);
  const files = engine.artifacts.filter((artifact) =>
    `${artifact.name} ${artifact.id} ${artifact.origin.runId ?? ''}`
      .toLowerCase()
      .includes(query.toLowerCase()),
  );
  const artifact = files.find((artifact) => artifact.id === selected);
  useEffect(() => {
    let current = true;
    setPreview('');
    setPreviewNotice('');
    setError('');
    setLocalError(false);
    setBusy(false);
    if (artifact && !engine.info)
      setPreviewNotice('Reconnect to the engine to verify and preview these bytes.');
    if (artifact && engine.info) {
      setBusy(true);
      void downloadArtifact(artifact.id)
        .then((result) => {
          if (current) {
            try {
              setPreview(decodeText(result.content));
            } catch {
              setPreviewNotice('Binary artifact. Export to inspect the preserved bytes.');
            }
          }
        })
        .catch((error) => {
          if (current) {
            setError(engineError(error).message);
            setLocalError(false);
          }
        })
        .finally(() => {
          if (current) setBusy(false);
        });
    }
    return () => {
      current = false;
    };
  }, [artifact?.id, engine.info]);
  return (
    <section className="page">
      <header className="page-heading">
        <div>
          <h1>{t('execution.engineArtifacts')}</h1>
          <p>{t('execution.registeredImmutableFilesWithCheckedSizesHashesAnd')}</p>
        </div>
        <label className="button">
          {t('execution.uploadInputFile')}
          <input
            hidden
            type="file"
            disabled={!engine.info}
            onChange={async (event) => {
              const file = event.target.files?.[0];
              event.target.value = '';
              if (!file) return;
              setError('');
              setLocalError(false);
              let localFailure = false;
              try {
                if (file.size > 64 * 1024 * 1024) {
                  localFailure = true;
                  throw new Error('Artifact exceeds 64 MiB.');
                }
                await engine.command({
                  op: 'upload',
                  file: {
                    path: file.name,
                    content: base64(new Uint8Array(await file.arrayBuffer())),
                  },
                  mediaType: file.type || 'application/octet-stream',
                  operationId: crypto.randomUUID(),
                });
              } catch (error) {
                setError(engineError(error).message);
                setLocalError(localFailure);
              }
            }}
            aria-label={t('execution.uploadEngineArtifact')}
          />
        </label>
      </header>
      <div className="list-toolbar">
        <label className="search-field">
          <Search size={16} />
          <input
            aria-label={t('execution.searchEngineArtifacts')}
            placeholder={t('execution.searchFiles')}
            value={query}
            onChange={(event) => setQuery(event.target.value)}
          />
        </label>
      </div>
      {error ? (
        <p className="form-error" role="alert">
          {localError ? t('execution.artifactExceeds64Mib') : error}
        </p>
      ) : null}
      {files.length ? (
        <div className="artifact-layout">
          <div className="artifact-grid">
            {files.map((file) => (
              <button
                key={file.id}
                className={`artifact-card ${selected === file.id ? 'selected' : ''}`}
                onClick={() => setSelected(file.id)}
              >
                <div className="artifact-thumbnail">
                  <FileBox size={36} />
                </div>
                <div className="artifact-info">
                  <strong>{file.name}</strong>
                  <span>{file.origin.runId ?? t('execution.uploadedInput')}</span>
                  <small>
                    {size(file.size, locale)} · {file.mediaType}
                  </small>
                </div>
              </button>
            ))}
          </div>
          {artifact ? (
            <aside className="artifact-inspector">
              <div className="inline spread">
                <h3>{artifact.name}</h3>
                <button
                  className="button small-button"
                  disabled={!engine.info || busy || !!error}
                  onClick={() => {
                    void exportEngineArtifact(artifact.id).catch((error) => {
                      setError(engineError(error).message);
                      setLocalError(false);
                    });
                  }}
                >
                  <Download size={14} />
                  {t('common.export')}
                </button>
              </div>
              <div className="detail-field">
                <span>SHA-256</span>
                <code className="hash">{artifact.sha256}</code>
              </div>
              <div className="detail-field">
                <span>{t('execution.origin')}</span>
                <code>{JSON.stringify(artifact.origin, null, 2)}</code>
              </div>
              <pre className="artifact-text">
                {busy
                  ? t('execution.verifyingBytes')
                  : previewNotice
                    ? message(previewNotice)
                    : preview}
              </pre>
            </aside>
          ) : null}
        </div>
      ) : (
        <Empty icon={<FileBox />} title={t('execution.noEngineArtifacts')}>
          {t('execution.uploadInputFilesOrInspectDeclaredOutputsFrom')}
        </Empty>
      )}
    </section>
  );
}
