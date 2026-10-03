import { useEffect, useState } from 'react';
import { Download, Search, FileBox } from 'lucide-react';
import { base64, decodeText } from '../lib/bytes';
import { engineError, exportEngineArtifact, downloadArtifact } from '../lib/engine/client';
import type { EngineController } from '../lib/engine/useEngine';
import { Empty, time, size } from './ui';

export function EngineArtifactsView({ engine }: { engine: EngineController }) {
  const [selected, setSelected] = useState('');
  const [query, setQuery] = useState('');
  const [preview, setPreview] = useState('');
  const [error, setError] = useState('');
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
    setError('');
    if (artifact) {
      setBusy(true);
      void downloadArtifact(artifact.id)
        .then((result) => {
          if (current) {
            try {
              setPreview(decodeText(result.content));
            } catch {
              setPreview('Binary artifact. Export to inspect the preserved bytes.');
            }
          }
        })
        .catch((error) => {
          if (current) setError(engineError(error).message);
        })
        .finally(() => {
          if (current) setBusy(false);
        });
    }
    return () => {
      current = false;
    };
  }, [artifact?.id]);
  return (
    <section className="page">
      <header className="page-heading">
        <div>
          <h1>Engine artifacts</h1>
          <p>Registered immutable files with checked sizes, hashes and provenance.</p>
        </div>
        <label className="button">
          Upload input file
          <input
            hidden
            type="file"
            onChange={async (event) => {
              const file = event.target.files?.[0];
              event.target.value = '';
              if (!file) return;
              setError('');
              try {
                if (file.size > 64 * 1024 * 1024) throw new Error('Artifact exceeds 64 MiB.');
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
              }
            }}
            aria-label="Upload engine artifact"
          />
        </label>
      </header>
      <div className="list-toolbar">
        <label className="search-field">
          <Search size={16} />
          <input
            aria-label="Search engine artifacts"
            placeholder="Search files…"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
          />
        </label>
      </div>
      {error ? (
        <p className="form-error" role="alert">
          {error}
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
                  <span>{file.origin.runId ?? 'Uploaded input'}</span>
                  <small>
                    {size(file.size)} · {file.mediaType}
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
                  disabled={busy || !!error}
                  onClick={() => {
                    void exportEngineArtifact(artifact.id).catch((error) =>
                      setError(engineError(error).message),
                    );
                  }}
                >
                  <Download size={14} />
                  Export
                </button>
              </div>
              <div className="detail-field">
                <span>SHA-256</span>
                <code className="hash">{artifact.sha256}</code>
              </div>
              <div className="detail-field">
                <span>Origin</span>
                <code>{JSON.stringify(artifact.origin, null, 2)}</code>
              </div>
              <pre className="artifact-text">{busy ? 'Verifying bytes…' : preview}</pre>
            </aside>
          ) : null}
        </div>
      ) : (
        <Empty icon={<FileBox />} title="No engine artifacts">
          Upload input files or inspect declared outputs from completed runs.
        </Empty>
      )}
    </section>
  );
}
