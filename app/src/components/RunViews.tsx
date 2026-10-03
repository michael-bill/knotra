import { useState } from 'react';
import {
  ArrowLeft,
  ArrowUpRight,
  Clock3,
  Play,
  Search,
  Inbox,
  Check,
  FileText,
  Download,
  XCircle,
  Braces,
  Fingerprint,
  ChevronRight,
  ChevronDown,
  ListFilter,
} from 'lucide-react';
import GraphView from './GraphView';
import SourceEditor from './SourceEditor';
import { Empty, Status, time, size } from './ui';
import { briefFor } from '../lib/demo';
import { parsePipeline } from '../lib/validation';
import type { Artifact, DemoRun, Json } from '../lib/types';

export function RunsView({
  runs,
  activeId,
  onSelect,
  onCancel,
  onReview,
  onStart,
}: {
  runs: DemoRun[];
  activeId?: string;
  onSelect: (id?: string) => void;
  onCancel: (id: string) => void;
  onReview: (id: string) => void;
  onStart: () => void;
}) {
  const [search, setSearch] = useState('');
  const [filter, setFilter] = useState('all');
  const [tab, setTab] = useState('timeline');
  const run = runs.find((r) => r.id === activeId);
  const pipeline = run ? parsePipeline(run.source) : undefined;
  if (run)
    return (
      <div className="run-detail">
        <div className="run-heading">
          <button className="text-button" onClick={() => onSelect(undefined)}>
            <ArrowLeft size={15} />
            All runs
          </button>
          <span className="demo-tag">DEMO RUN</span>
        </div>
        <header className="page-heading">
          <div>
            <h1>{run.topic}</h1>
            <p>
              {run.title}
              <span className="separator">/</span>
              {time(run.createdAt)}
              <span className="separator">/</span>
              <code>{run.id.slice(0, 8)}</code>
            </p>
          </div>
          <div className="heading-actions">
            <Status status={run.status} />
            {run.status === 'waiting_human' ? (
              <button className="button primary" onClick={() => onReview(run.id)}>
                Review request
                <ArrowUpRight size={15} />
              </button>
            ) : null}
            {['running', 'waiting_human'].includes(run.status) ? (
              <button className="button" onClick={() => onCancel(run.id)}>
                <XCircle size={15} />
                Cancel
              </button>
            ) : null}
          </div>
        </header>
        <div className="notice small">
          This guided demo uses sample outputs. No providers, tools or sandbox commands are called.
        </div>
        <div className="run-content">
          {pipeline ? (
            <div className="run-graph">
              <GraphView graph={pipeline.spec} statuses={run.nodes} onSelect={() => {}} />
            </div>
          ) : null}
          <div className="run-bottom">
            <div className="underline-tabs">
              {['timeline', 'inputs', 'outputs', 'snapshot'].map((t) => (
                <button key={t} className={tab === t ? 'active' : ''} onClick={() => setTab(t)}>
                  {t[0].toUpperCase() + t.slice(1)}
                </button>
              ))}
            </div>
            {tab === 'timeline' ? (
              <div className="timeline">
                {run.events.map((e) => (
                  <div key={e.id} className="timeline-event">
                    <span className="timeline-marker" />
                    <time>
                      {new Date(e.at).toLocaleTimeString([], {
                        hour: '2-digit',
                        minute: '2-digit',
                        second: '2-digit',
                      })}
                    </time>
                    <div>
                      {e.node ? <strong>{e.node}</strong> : null}
                      <p>{e.message}</p>
                    </div>
                  </div>
                ))}
              </div>
            ) : tab === 'snapshot' ? (
              <div className="snapshot-editor">
                <SourceEditor source={run.source} readOnly label="Immutable run snapshot" />
              </div>
            ) : (
              <pre className="json-view">
                {JSON.stringify(tab === 'inputs' ? run.inputs : run.outputs, null, 2)}
              </pre>
            )}
          </div>
        </div>
      </div>
    );
  const filtered = runs.filter(
    (r) =>
      (filter === 'all' || r.status === filter) &&
      `${r.topic} ${r.id} ${r.title}`.toLowerCase().includes(search.toLowerCase()),
  );
  return (
    <section className="page">
      <header className="page-heading">
        <div>
          <div className="breadcrumb">
            Workspace
            <ChevronRight size={13} />
            History
          </div>
          <h1>Run history</h1>
          <p>Follow the work, inspect the outputs, and pick up where you left off.</p>
        </div>
        <button className="button primary" onClick={onStart}>
          <Play size={15} />
          Try the guided demo
        </button>
      </header>
      <div className="summary-cards">
        <div>
          <span>Total runs</span>
          <strong>{runs.length}</strong>
          <small>Local demo history</small>
        </div>
        <div>
          <span>In progress</span>
          <strong>{runs.filter((r) => r.status === 'running').length}</strong>
          <small>Work is moving forward</small>
        </div>
        <div>
          <span>Needs your attention</span>
          <strong>{runs.filter((r) => r.status === 'waiting_human').length}</strong>
          <small>Human review requests</small>
        </div>
        <div>
          <span>Completed</span>
          <strong>{runs.filter((r) => r.status === 'succeeded').length}</strong>
          <small>Outputs ready to inspect</small>
        </div>
      </div>
      <div className="list-toolbar runs-toolbar">
        <label className="search-field">
          <Search size={16} />
          <input
            aria-label="Search runs"
            placeholder="Search runs…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
        </label>
        <label className="status-filter">
          <ListFilter size={16} aria-hidden="true" />
          <select
            aria-label="Filter runs by status"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          >
            <option value="all">All statuses</option>
            <option value="running">Running</option>
            <option value="waiting_human">Needs review</option>
            <option value="succeeded">Completed</option>
            <option value="cancelled">Cancelled</option>
          </select>
          <ChevronDown size={15} aria-hidden="true" />
        </label>
      </div>
      {filtered.length ? (
        <div className="run-table">
          <div className="table-header">
            <span>PIPELINE / TOPIC</span>
            <span>STATUS</span>
            <span>STARTED</span>
            <span>MODE</span>
          </div>
          {filtered.map((r) => (
            <button className="table-row" key={r.id} onClick={() => onSelect(r.id)}>
              <span className="run-name">
                <span className="run-list-icon">
                  <Play size={17} />
                </span>
                <span>
                  <strong>{r.topic}</strong>
                  <small>
                    {r.title} · {r.id.slice(0, 8)}
                  </small>
                </span>
              </span>
              <Status status={r.status} />
              <span className="muted small">{time(r.createdAt)}</span>
              <span className="inline">
                <span className="demo-tag">Demo</span>
                <ChevronRight size={15} />
              </span>
            </button>
          ))}
        </div>
      ) : (
        <Empty
          icon={<Clock3 size={30} />}
          title={runs.length ? 'No matching runs' : 'No runs yet'}
          action={
            !runs.length ? (
              <button className="button" onClick={onStart}>
                Explore the demo
                <ArrowUpRight size={15} />
              </button>
            ) : undefined
          }
        >
          {runs.length
            ? 'Try a different search or status filter.'
            : 'Start a Research brief demo to see events, human review and artifacts in one place.'}
        </Empty>
      )}
    </section>
  );
}

export function InboxView({
  runs,
  onRespond,
  onRun,
}: {
  runs: DemoRun[];
  onRespond: (id: string, request: string, response: Record<string, Json>) => void;
  onRun: (id: string) => void;
}) {
  const waiting = runs.filter((r) => r.status === 'waiting_human');
  const [selected, setSelected] = useState<string>();
  const [feedback, setFeedback] = useState('');
  const [advanced, setAdvanced] = useState(false);
  const [json, setJson] = useState('{\n  "feedback": "Looks good. Ready to share."\n}');
  const [error, setError] = useState('');
  const run = waiting.find((r) => r.id === selected) ?? waiting[0];
  function submit() {
    if (!run?.requestId) return;
    try {
      const response = advanced ? JSON.parse(json) : { feedback };
      onRespond(run.id, run.requestId, response);
      setFeedback('');
      setError('');
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }
  return (
    <section className="page inbox-page">
      <header className="page-heading">
        <div>
          <div className="breadcrumb">
            Workspace
            <ChevronRight size={13} />
            Human in the loop
          </div>
          <h1>Human review</h1>
          <p>Review the context. Your response belongs to one specific saved request.</p>
        </div>
        <span className="count-pill">
          {waiting.length} {waiting.length === 1 ? 'open request' : 'open requests'}
        </span>
      </header>
      {run ? (
        <div className="inbox-layout">
          <div className="request-list">
            {waiting.map((r) => (
              <button
                key={r.id}
                className={r.id === run.id ? 'active' : ''}
                onClick={() => {
                  setSelected(r.id);
                  setError('');
                  setFeedback('');
                }}
              >
                <span className="request-icon">
                  <Inbox size={17} />
                </span>
                <strong>{r.topic}</strong>
                <small>Research brief · review</small>
                <time>{time(r.updatedAt)}</time>
                <span className="demo-tag">Demo</span>
              </button>
            ))}
          </div>
          <div className="review-card">
            <header>
              <div>
                <span className="eyebrow">HUMAN REVIEW</span>
                <h2>Review the generated brief</h2>
              </div>
              <button className="text-button" onClick={() => onRun(run.id)}>
                View run
                <ArrowUpRight size={14} />
              </button>
            </header>
            <p className="review-prompt">
              Review the brief and return your feedback. Your response is saved with this run.
            </p>
            <div className="brief-preview">
              {briefFor(run)
                .split('\n')
                .map((line, i) =>
                  line.startsWith('# ') ? (
                    <h2 key={i}>{line.slice(2)}</h2>
                  ) : line.startsWith('## ') ? (
                    <h3 key={i}>{line.slice(3)}</h3>
                  ) : line.startsWith('- ') ? (
                    <p className="brief-bullet" key={i}>
                      • {line.slice(2)}
                    </p>
                  ) : line ? (
                    <p key={i}>{line}</p>
                  ) : null,
                )}
            </div>
            <div className="response-form">
              <div className="inline spread">
                <label htmlFor="review-feedback">Your feedback</label>
                <button className="text-button small" onClick={() => setAdvanced(!advanced)}>
                  <Braces size={13} />
                  {advanced ? 'Simple response' : 'JSON response'}
                </button>
              </div>
              {advanced ? (
                <textarea
                  aria-label="Review JSON response"
                  rows={5}
                  value={json}
                  onChange={(e) => setJson(e.target.value)}
                  className="code-input"
                />
              ) : (
                <textarea
                  id="review-feedback"
                  placeholder="What should be kept, clarified or improved?"
                  rows={4}
                  value={feedback}
                  onChange={(e) => setFeedback(e.target.value)}
                />
              )}
              {error ? (
                <p className="form-error" role="alert">
                  {error}
                </p>
              ) : null}
              <div className="response-footer">
                <small>
                  <Fingerprint size={13} />
                  Request {run.requestId?.slice(0, 8)}
                </small>
                <button
                  className="button primary"
                  onClick={submit}
                  disabled={!advanced && !feedback.trim()}
                >
                  <Check size={16} />
                  Submit response
                </button>
              </div>
            </div>
          </div>
        </div>
      ) : (
        <Empty icon={<Inbox size={30} />} title="You're all caught up.">
          When a pipeline reaches a human node, the saved request will appear here. Demo requests
          survive a reload.
        </Empty>
      )}
    </section>
  );
}

export function ArtifactsView({
  runs,
  onExport,
}: {
  runs: DemoRun[];
  onExport: (artifact: Artifact) => void;
}) {
  const artifacts = runs.flatMap((r) => r.artifacts.map((a) => ({ ...a, run: r })));
  const [selected, setSelected] = useState<string>();
  const [query, setQuery] = useState('');
  const filtered = artifacts.filter((a) =>
    `${a.name} ${a.run.topic}`.toLowerCase().includes(query.toLowerCase()),
  );
  const artifact = filtered.find((a) => a.id === selected);
  return (
    <section className="page">
      <header className="page-heading">
        <div>
          <div className="breadcrumb">
            Workspace
            <ChevronRight size={13} />
            Outputs
          </div>
          <h1>Artifacts</h1>
          <p>Declared files, preserved bytes, and a traceable origin.</p>
        </div>
        <span className="count-pill">{artifacts.length} artifacts</span>
      </header>
      <div className="list-toolbar">
        <label className="search-field">
          <Search size={16} />
          <input
            placeholder="Search artifacts…"
            aria-label="Search artifacts"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
        <span className="small muted">Sample artifacts from demo runs</span>
      </div>
      {filtered.length ? (
        <div className="artifact-layout">
          <div className="artifact-grid">
            {filtered.map((a) => (
              <button
                key={a.id}
                className={`artifact-card ${selected === a.id ? 'selected' : ''}`}
                onClick={() => setSelected(a.id)}
              >
                <div className="artifact-thumbnail">
                  <FileText size={38} />
                  <span>MD</span>
                </div>
                <div className="artifact-info">
                  <strong>{a.name}</strong>
                  <span>{a.run.topic}</span>
                  <small>
                    {size(a.size)} · {a.mediaType}
                  </small>
                </div>
                <div className="artifact-footer">
                  <span className="demo-tag">Demo</span>
                  <ArrowUpRight size={15} />
                </div>
              </button>
            ))}
          </div>
          {artifact ? (
            <aside className="artifact-inspector">
              <div className="inline spread">
                <h3>{artifact.name}</h3>
                <button className="button small-button" onClick={() => onExport(artifact)}>
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
                <strong>{artifact.run.topic}</strong>
                <small>Demo run · {artifact.run.id.slice(0, 8)}</small>
              </div>
              <pre className="artifact-text">{artifact.content}</pre>
            </aside>
          ) : null}
        </div>
      ) : (
        <Empty
          icon={<FileText size={30} />}
          title={artifacts.length ? 'No matching artifacts' : 'Your outputs live here'}
        >
          {artifacts.length
            ? 'Try a different file name or research topic.'
            : 'Complete a Research brief demo and its Markdown artifact will appear here, with bytes, size and a SHA-256 hash.'}
        </Empty>
      )}
    </section>
  );
}
