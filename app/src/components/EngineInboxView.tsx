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
  const [selected, setSelected] = useState('');
  const requests = engine.requests.filter((request) => request.status === 'open');
  const request = requests.find((request) => request.id === selected) ?? requests[0];
  return (
    <section className="page inbox-page">
      <header className="page-heading">
        <div>
          <h1>Engine inbox</h1>
          <p>Each response is addressed to a saved request and checked by the engine.</p>
        </div>
        <span className="count-pill">{requests.length} open requests</span>
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
                <time>{time(request.createdAt)}</time>
              </button>
            ))}
          </div>
          <EngineResponse key={request.id} request={request} engine={engine} onRun={onRun} />
        </div>
      ) : (
        <Empty icon={<Inbox />} title="No open engine requests">
          Saved human requests appear here when the engine pauses a node for your response.
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
  const [value, setValue] = useState('{}');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  async function submit() {
    setError('');
    setBusy(true);
    try {
      const outputs = JSON.parse(value);
      if (!outputs || typeof outputs !== 'object' || Array.isArray(outputs))
        throw new Error('Response must be an object of output ports.');
      if (!new Validator(request.responseSchema, '2020-12', false).validate(outputs).valid)
        throw new Error('Response does not match the requested JSON schema.');
      await engine.command({
        op: 'respond',
        requestId: request.id,
        outputs,
        operationId: crypto.randomUUID(),
      });
    } catch (error) {
      setError(engineError(error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="review-card">
      <header>
        <div>
          <span className="eyebrow">HUMAN REQUEST</span>
          <h2>{request.instanceId}</h2>
        </div>
        <button className="text-button" onClick={() => onRun(request.runId)}>
          View run
        </button>
      </header>
      <p className="review-prompt">{request.prompt}</p>
      <p className="small muted">Deadline: {time(request.deadline)}</p>
      <pre className="json-view">{JSON.stringify(request.inputs, null, 2)}</pre>
      <details>
        <summary>Response schema</summary>
        <pre className="json-view">{JSON.stringify(request.responseSchema, null, 2)}</pre>
      </details>
      <div className="response-form">
        <label className="field">
          Response output ports
          <textarea
            aria-label="Engine review response"
            className="code-input"
            rows={7}
            value={value}
            onChange={(event) => setValue(event.target.value)}
          />
        </label>
        {error ? (
          <p className="form-error" role="alert">
            {error}
          </p>
        ) : null}
        <div className="response-footer">
          <small>Request {request.id}</small>
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
            Submit response
          </button>
        </div>
      </div>
    </div>
  );
}
