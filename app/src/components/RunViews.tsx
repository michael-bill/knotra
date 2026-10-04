import { useI18n } from '../lib/i18n';
import { demoStatusLabels, executionTabLabels } from '../lib/executionLabels';
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
  const { t, locale } = useI18n();
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
            {t('execution.allRuns')}
          </button>
          <span className="demo-tag">{t('execution.demoRun')}</span>
        </div>
        <header className="page-heading">
          <div>
            <h1>{run.topic}</h1>
            <p>
              {run.title}
              <span className="separator">/</span>
              {time(run.createdAt, locale)}
              <span className="separator">/</span>
              <code>{run.id.slice(0, 8)}</code>
            </p>
          </div>
          <div className="heading-actions">
            <Status status={run.status} />
            {run.status === 'waiting_human' ? (
              <button className="button primary" onClick={() => onReview(run.id)}>
                {t('execution.reviewRequest')}
                <ArrowUpRight size={15} />
              </button>
            ) : null}
            {['running', 'waiting_human'].includes(run.status) ? (
              <button className="button" onClick={() => onCancel(run.id)}>
                <XCircle size={15} />
                {t('common.cancel')}
              </button>
            ) : null}
          </div>
        </header>
        <div className="notice small">
          {t('execution.thisGuidedDemoUsesSampleOutputsNoProviders')}
        </div>
        <div className="run-content">
          {pipeline ? (
            <div className="run-graph">
              <GraphView graph={pipeline.spec} statuses={run.nodes} onSelect={() => {}} />
            </div>
          ) : null}
          <div className="run-bottom">
            <div className="underline-tabs">
              {['timeline', 'inputs', 'outputs', 'snapshot'].map((tabName) => (
                <button
                  key={tabName}
                  className={tab === tabName ? 'active' : ''}
                  onClick={() => setTab(tabName)}
                >
                  {t(executionTabLabels[tabName] ?? tabName)}
                </button>
              ))}
            </div>
            {tab === 'timeline' ? (
              <div className="timeline">
                {run.events.map((e) => (
                  <div key={e.id} className="timeline-event">
                    <span className="timeline-marker" />
                    <time>
                      {new Date(e.at).toLocaleTimeString(locale === 'ru' ? 'ru-RU' : 'en-US', {
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
                <SourceEditor
                  source={run.source}
                  readOnly
                  label={t('execution.immutableRunSnapshot')}
                />
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
            {t('resources.workspace')}
            <ChevronRight size={13} />
            {t('execution.history')}
          </div>
          <h1>{t('execution.runHistory')}</h1>
          <p>{t('execution.followTheWorkInspectTheOutputsAndPick')}</p>
        </div>
        <button className="button primary" onClick={onStart}>
          <Play size={15} />
          {t('execution.tryTheGuidedDemo')}
        </button>
      </header>
      <div className="summary-cards">
        <div>
          <span>{t('execution.totalRuns')}</span>
          <strong>{runs.length}</strong>
          <small>{t('execution.localDemoHistory')}</small>
        </div>
        <div>
          <span>{t('execution.inProgress')}</span>
          <strong>{runs.filter((r) => r.status === 'running').length}</strong>
          <small>{t('execution.workIsMovingForward')}</small>
        </div>
        <div>
          <span>{t('execution.needsYourAttention')}</span>
          <strong>{runs.filter((r) => r.status === 'waiting_human').length}</strong>
          <small>{t('execution.humanReviewRequests')}</small>
        </div>
        <div>
          <span>{t('execution.completed')}</span>
          <strong>{runs.filter((r) => r.status === 'succeeded').length}</strong>
          <small>{t('execution.outputsReadyToInspect')}</small>
        </div>
      </div>
      <div className="list-toolbar runs-toolbar">
        <label className="search-field">
          <Search size={16} />
          <input
            aria-label={t('execution.searchRuns')}
            placeholder={t('execution.searchRuns2')}
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
        </label>
        <label className="status-filter">
          <ListFilter size={16} aria-hidden="true" />
          <select
            aria-label={t('execution.filterRunsByStatus')}
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          >
            <option value="all">{t(demoStatusLabels.all)}</option>
            <option value="running">{t(demoStatusLabels.running)}</option>
            <option value="waiting_human">{t(demoStatusLabels.waiting_human)}</option>
            <option value="succeeded">{t(demoStatusLabels.succeeded)}</option>
            <option value="cancelled">{t(demoStatusLabels.cancelled)}</option>
          </select>
          <ChevronDown size={15} aria-hidden="true" />
        </label>
      </div>
      {filtered.length ? (
        <div className="run-table">
          <div className="table-header">
            <span>{t('execution.pipelineTopic')}</span>
            <span>{t('execution.status')}</span>
            <span>{t('execution.started')}</span>
            <span>{t('execution.mode')}</span>
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
              <span className="muted small">{time(r.createdAt, locale)}</span>
              <span className="inline">
                <span className="demo-tag">{t('execution.demo')}</span>
                <ChevronRight size={15} />
              </span>
            </button>
          ))}
        </div>
      ) : (
        <Empty
          icon={<Clock3 size={30} />}
          title={runs.length ? t('execution.noMatchingRuns') : t('execution.noRunsYet')}
          action={
            !runs.length ? (
              <button className="button" onClick={onStart}>
                {t('execution.exploreTheDemo')}
                <ArrowUpRight size={15} />
              </button>
            ) : undefined
          }
        >
          {runs.length
            ? t('execution.tryADifferentSearchOrStatusFilter')
            : t('execution.startAResearchBriefDemoToSeeEvents')}
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
  const { t, locale, message } = useI18n();
  const waiting = runs.filter((r) => r.status === 'waiting_human');
  const [selected, setSelected] = useState<string>();
  const [feedback, setFeedback] = useState('');
  const [advanced, setAdvanced] = useState(false);
  const [json, setJson] = useState('{\n  "feedback": "Looks good. Ready to share."\n}');
  const [error, setError] = useState('');
  const [jsonError, setJsonError] = useState(false);
  const run = waiting.find((r) => r.id === selected) ?? waiting[0];
  function submit() {
    if (!run?.requestId) return;
    try {
      const response = advanced ? JSON.parse(json) : { feedback };
      onRespond(run.id, run.requestId, response);
      setFeedback('');
      setError('');
      setJsonError(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setJsonError(e instanceof SyntaxError);
    }
  }
  return (
    <section className="page inbox-page">
      <header className="page-heading">
        <div>
          <div className="breadcrumb">
            {t('resources.workspace')}
            <ChevronRight size={13} />
            {t('execution.humanInTheLoop')}
          </div>
          <h1>{t('execution.humanReview')}</h1>
          <p>{t('execution.reviewTheContextYourResponseBelongsToOne')}</p>
        </div>
        <span className="count-pill">
          {t(waiting.length === 1 ? 'execution.countOpenRequest' : 'execution.countOpenRequests', {
            count: waiting.length,
          })}
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
                <small>{t('execution.researchBriefReview')}</small>
                <time>{time(r.updatedAt, locale)}</time>
                <span className="demo-tag">{t('execution.demo')}</span>
              </button>
            ))}
          </div>
          <div className="review-card">
            <header>
              <div>
                <span className="eyebrow">{t('execution.humanReview2')}</span>
                <h2>{t('execution.reviewTheGeneratedBrief')}</h2>
              </div>
              <button className="text-button" onClick={() => onRun(run.id)}>
                {t('execution.viewRun')}
                <ArrowUpRight size={14} />
              </button>
            </header>
            <p className="review-prompt">
              {t('execution.reviewTheBriefAndReturnYourFeedbackYour')}
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
                <label htmlFor="review-feedback">{t('execution.yourFeedback')}</label>
                <button className="text-button small" onClick={() => setAdvanced(!advanced)}>
                  <Braces size={13} />
                  {advanced ? t('execution.simpleResponse') : t('execution.jsonResponse')}
                </button>
              </div>
              {advanced ? (
                <textarea
                  aria-label={t('execution.reviewJsonResponse')}
                  rows={5}
                  value={json}
                  onChange={(e) => setJson(e.target.value)}
                  className="code-input"
                />
              ) : (
                <textarea
                  id="review-feedback"
                  placeholder={t('execution.whatShouldBeKeptClarifiedOrImproved')}
                  rows={4}
                  value={feedback}
                  onChange={(e) => setFeedback(e.target.value)}
                />
              )}
              {error ? (
                <p className="form-error" role="alert">
                  {jsonError
                    ? t('execution.invalidJsonMessage', { message: error })
                    : error
                        .split('\n')
                        .map((line) => message(line))
                        .join('\n')}
                </p>
              ) : null}
              <div className="response-footer">
                <small>
                  <Fingerprint size={13} />
                  {t('execution.requestId', { id: run.requestId?.slice(0, 8) ?? '' })}
                </small>
                <button
                  className="button primary"
                  onClick={submit}
                  disabled={!advanced && !feedback.trim()}
                >
                  <Check size={16} />
                  {t('execution.submitResponse')}
                </button>
              </div>
            </div>
          </div>
        </div>
      ) : (
        <Empty icon={<Inbox size={30} />} title={t('execution.youReAllCaughtUp')}>
          {t('execution.whenAPipelineReachesAHumanNodeThe')}
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
  const { t, locale } = useI18n();
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
            {t('resources.workspace')}
            <ChevronRight size={13} />
            {t('execution.outputs')}
          </div>
          <h1>{t('navigation.artifacts')}</h1>
          <p>{t('execution.declaredFilesPreservedBytesAndATraceableOrigin')}</p>
        </div>
        <span className="count-pill">
          {t('execution.countArtifacts', { count: artifacts.length })}
        </span>
      </header>
      <div className="list-toolbar">
        <label className="search-field">
          <Search size={16} />
          <input
            placeholder={t('execution.searchArtifacts2')}
            aria-label={t('execution.searchArtifacts')}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
        <span className="small muted">{t('execution.sampleArtifactsFromDemoRuns')}</span>
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
                    {size(a.size, locale)} · {a.mediaType}
                  </small>
                </div>
                <div className="artifact-footer">
                  <span className="demo-tag">{t('execution.demo')}</span>
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
                  {t('common.export')}
                </button>
              </div>
              <div className="detail-field">
                <span>SHA-256</span>
                <code className="hash">{artifact.sha256}</code>
              </div>
              <div className="detail-field">
                <span>{t('execution.origin')}</span>
                <strong>{artifact.run.topic}</strong>
                <small>{t('execution.demoRunId', { id: artifact.run.id.slice(0, 8) })}</small>
              </div>
              <pre className="artifact-text">{artifact.content}</pre>
            </aside>
          ) : null}
        </div>
      ) : (
        <Empty
          icon={<FileText size={30} />}
          title={
            artifacts.length
              ? t('execution.noMatchingArtifacts')
              : t('execution.yourOutputsLiveHere')
          }
        >
          {artifacts.length
            ? t('execution.tryADifferentFileNameOrResearchTopic')
            : t('execution.completeAResearchBriefDemoAndItsMarkdown')}
        </Empty>
      )}
    </section>
  );
}
