import { RefreshCw } from 'lucide-react';
import type { EngineController } from '../lib/engine/useEngine';
import { useI18n } from '../lib/i18n';
import { executionCommandLabels } from '../lib/executionLabels';

export function EngineBanner({
  engine,
  onSettings,
}: {
  engine: EngineController;
  onSettings: () => void;
}) {
  const { t } = useI18n();
  return (
    <div className="engine-banner">
      <span className={`dot ${engine.info ? 'green' : 'amber'}`} />
      <span>
        {engine.info
          ? t('execution.engineVersionStatus', {
              version: engine.info.version,
              status: engine.error
                ? t('execution.unavailableCachedData')
                : engine.syncing
                  ? t('execution.syncing')
                  : t('resources.connected'),
            })
          : t('execution.connectAnEngineToExecuteYourWorkflows')}
      </span>
      <button
        className="text-button"
        onClick={engine.info ? () => void engine.refresh() : onSettings}
      >
        {engine.info ? (
          <>
            <RefreshCw size={14} />
            {t('common.refresh')}
          </>
        ) : (
          t('execution.connectInSettings')
        )}
      </button>
      {engine.error ? (
        <p className="form-error" role="alert">
          {engine.error}
          {engine.info ? ` ${t('execution.showingTheLastReceivedData')}` : ''}
        </p>
      ) : null}
      {engine.pending.length ? (
        <div className="pending-operations">
          <strong>
            {t('execution.countCommandSAwaitingConfirmation', { count: engine.pending.length })}
          </strong>
          <p>{t('execution.reconcileTheseSavedCommandsWithTheirOriginalOperation')}</p>
          {engine.pending.map((operation) => (
            <button
              className="button small-button"
              key={operation.id}
              onClick={() => {
                void engine.command(operation.request).catch(() => {});
              }}
            >
              {t('execution.reconcileOperationId', {
                operation: t(executionCommandLabels[operation.request.op] ?? operation.request.op),
                id: operation.id.slice(0, 8),
              })}
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}
