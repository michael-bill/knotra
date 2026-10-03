import { RefreshCw } from 'lucide-react';
import type { EngineController } from '../lib/engine/useEngine';

export function EngineBanner({
  engine,
  onSettings,
}: {
  engine: EngineController;
  onSettings: () => void;
}) {
  return (
    <div className="engine-banner">
      <span className={`dot ${engine.info ? 'green' : 'amber'}`} />
      <span>
        {engine.info
          ? `Engine ${engine.info.version} · ${engine.error ? 'Unavailable · cached data' : engine.syncing ? 'Syncing…' : 'Connected'}`
          : 'Connect an engine to execute your workflows.'}
      </span>
      <button
        className="text-button"
        onClick={engine.info ? () => void engine.refresh() : onSettings}
      >
        {engine.info ? (
          <>
            <RefreshCw size={14} />
            Refresh
          </>
        ) : (
          'Connect in Settings'
        )}
      </button>
      {engine.error ? (
        <p className="form-error" role="alert">
          {engine.error}
          {engine.info ? ' Showing the last received data.' : ''}
        </p>
      ) : null}
      {engine.pending.length ? (
        <div className="pending-operations">
          <strong>{engine.pending.length} command(s) awaiting confirmation</strong>
          <p>
            Reconcile these saved commands with their original operation IDs before creating
            replacements.
          </p>
          {engine.pending.map((operation) => (
            <button
              className="button small-button"
              key={operation.id}
              onClick={() => {
                void engine.command(operation.request).catch(() => {});
              }}
            >
              Reconcile {operation.request.op} · {operation.id.slice(0, 8)}
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}
