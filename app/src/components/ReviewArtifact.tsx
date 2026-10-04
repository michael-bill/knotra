import { useState } from 'react';
import { Download, FileBox } from 'lucide-react';
import { decodeText } from '../lib/bytes';
import { downloadArtifact, engineError, exportEngineArtifact } from '../lib/engine/client';
import type { ContextArtifact } from '../lib/engine/types';
import { useI18n } from '../lib/i18n';
import { size } from './ui';

export function ReviewArtifact({
  name,
  artifact,
  connected,
}: {
  name: string;
  artifact: ContextArtifact;
  connected: boolean;
}) {
  const { t, locale } = useI18n();
  const [preview, setPreview] = useState('');
  const [notice, setNotice] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  async function inspect() {
    setBusy(true);
    setError('');
    setPreview('');
    setNotice('');
    try {
      const result = await downloadArtifact(artifact.id);
      if (result.artifact.sha256 !== artifact.sha256 || result.artifact.size !== artifact.size)
        throw new Error('Artifact size or SHA-256 does not match engine metadata.');
      if (artifact.mediaType.startsWith('text/') || artifact.mediaType === 'application/json') {
        const text = decodeText(result.content);
        setPreview(text.slice(0, 24576));
        if (text.length > 24576) setNotice(t('history.previewLimited'));
      } else setNotice(t('execution.binaryArtifactExportToInspectThePreservedBytes'));
    } catch (error) {
      setError(engineError(error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="review-artifact">
      <div className="execution-artifact">
        <FileBox size={16} />
        <span>
          <strong>{name}</strong>
          <small>
            {artifact.mediaType} · {size(artifact.size, locale)}
          </small>
        </span>
        <button
          className="text-button"
          disabled={!connected || busy}
          onClick={() => void inspect()}
        >
          {t('execution.previewReviewFile', { name })}
        </button>
        <button
          className="icon-button"
          aria-label={t('observe.downloadArtifact', { name })}
          disabled={!connected || busy}
          onClick={async () => {
            setBusy(true);
            setError('');
            try {
              await exportEngineArtifact(artifact.id);
            } catch (error) {
              setError(engineError(error).message);
            } finally {
              setBusy(false);
            }
          }}
        >
          <Download size={15} />
        </button>
      </div>
      {preview ? <pre className="json-view">{preview}</pre> : null}
      {notice ? <p className="small muted">{notice}</p> : null}
      {error ? (
        <p className="form-error" role="alert">
          {error}
        </p>
      ) : null}
    </section>
  );
}
