import { useI18n } from '../lib/i18n';
import { useState } from 'react';
import { Inbox } from 'lucide-react';
import { Validator } from '@cfworker/json-schema';
import { engineError } from '../lib/engine/client';
import type { EngineRequest } from '../lib/engine/types';
import type { EngineController } from '../lib/engine/useEngine';
import { Empty, time } from './ui';

export function EngineInboxView({
  engine,
  onRun,
}: {
  engine: EngineController;
  onRun: (id: string) => void;
}) {
  const { t, locale } = useI18n();
  const [selected, setSelected] = useState('');
  const requests = engine.requests.filter((request) => request.status === 'open');
  const request = requests.find((request) => request.id === selected) ?? requests[0];
  return (
    <section className="page inbox-page">
      <header className="page-heading">
        <div>
          <h1>{t('execution.engineInbox')}</h1>
          <p>{t('execution.eachResponseIsAddressedToASavedRequest')}</p>
        </div>
        <span className="count-pill">
          {t('execution.countOpenRequests', { count: requests.length })}
        </span>
      </header>
      {request ? (
        <div className="inbox-layout">
          <div className="request-list">
            {requests.map((request) => (
              <button
                key={request.id}
                className={selected === request.id ? 'active' : ''}
                onClick={() => setSelected(request.id)}
              >
                <Inbox size={17} />
                <strong>{request.instanceId}</strong>
                <small>{request.runId}</small>
                <time>{time(request.createdAt, locale)}</time>
              </button>
            ))}
          </div>
          <EngineResponse key={request.id} request={request} engine={engine} onRun={onRun} />
        </div>
      ) : (
        <Empty icon={<Inbox />} title={t('execution.noOpenEngineRequests')}>
          {t('execution.savedHumanRequestsAppearHereWhenTheEngine')}
        </Empty>
      )}
    </section>
  );
}

function EngineResponse({
  request,
  engine,
  onRun,
}: {
  request: EngineRequest;
  engine: EngineController;
  onRun: (id: string) => void;
}) {
  const { t, locale, message } = useI18n();
  const [value, setValue] = useState('{}');
  const [error, setError] = useState('');
  const [localError, setLocalError] = useState('');
  const [jsonError, setJsonError] = useState(false);
  const [busy, setBusy] = useState(false);
  async function submit() {
    setError('');
    setLocalError('');
    setJsonError(false);
    setBusy(true);
    let localFailure = '';
    try {
      const outputs = JSON.parse(value);
      if (!outputs || typeof outputs !== 'object' || Array.isArray(outputs)) {
        localFailure = 'Response must be an object of output ports.';
        throw new Error('Response must be an object of output ports.');
      }
      if (!new Validator(request.responseSchema, '2020-12', false).validate(outputs).valid) {
        localFailure = 'Response does not match the requested JSON schema.';
        throw new Error('Response does not match the requested JSON schema.');
      }
      await engine.command({
        op: 'respond',
        requestId: request.id,
        outputs,
        operationId: crypto.randomUUID(),
      });
    } catch (error) {
      setError(engineError(error).message);
      setLocalError(localFailure);
      setJsonError(error instanceof SyntaxError);
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="review-card">
      <header>
        <div>
          <span className="eyebrow">{t('execution.humanRequest')}</span>
          <h2>{request.instanceId}</h2>
        </div>
        <button className="text-button" onClick={() => onRun(request.runId)}>
          {t('execution.viewRun')}
        </button>
      </header>
      <p className="review-prompt">{request.prompt}</p>
      <p className="small muted">
        {t('execution.deadlineTime', { time: time(request.deadline, locale) })}
      </p>
      <pre className="json-view">{JSON.stringify(request.inputs, null, 2)}</pre>
      <details>
        <summary>{t('execution.responseSchema')}</summary>
        <pre className="json-view">{JSON.stringify(request.responseSchema, null, 2)}</pre>
      </details>
      <div className="response-form">
        <label className="field">
          {t('execution.responseOutputPorts')}
          <textarea
            aria-label={t('execution.engineReviewResponse')}
            className="code-input"
            rows={7}
            value={value}
            onChange={(event) => setValue(event.target.value)}
          />
        </label>
        {error ? (
          <p className="form-error" role="alert">
            {localError
              ? message(localError)
              : jsonError
                ? t('execution.invalidJsonMessage', { message: error })
                : error}
          </p>
        ) : null}
        <div className="response-footer">
          <small>{t('execution.requestId', { id: request.id })}</small>
          <button
            className="button primary"
            disabled={
              !engine.info ||
              busy ||
              engine.pending.some(
                (item) => item.request.op === 'respond' && item.request.requestId === request.id,
              )
            }
            onClick={() => void submit()}
          >
            {t('execution.submitResponse')}
          </button>
        </div>
      </div>
    </div>
  );
}
