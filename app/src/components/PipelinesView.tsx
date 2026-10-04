import { useMemo, useState } from 'react';
import { ArrowRight, FolderOpen, Plus, Search, Workflow } from 'lucide-react';
import { examples, type Example } from '../lib/examples';
import { useI18n } from '../lib/i18n';
import { parsePipeline } from '../lib/validation';
import type { NodeKind, Workspace } from '../lib/types';
import { Empty, NodeIcon, nodeMeta, time } from './ui';
import './PipelinesView.css';

export function PipelinesView({
  workspaces,
  onOpen,
  onCreate,
  onImport,
  busy,
}: {
  workspaces: Workspace[];
  onOpen: (id: string) => void;
  onCreate: () => void;
  onImport: () => void;
  busy: boolean;
}) {
  const { t, locale } = useI18n();
  const [query, setQuery] = useState('');
  const [sort, setSort] = useState('updated');
  const pipelines = useMemo(
    () =>
      workspaces
        .map((workspace) => {
          const pipeline = parsePipeline(workspace.source);
          return {
            workspace,
            pipeline,
            title: pipeline?.metadata.title ?? pipeline?.metadata.name ?? t('editor.pipelineDraft'),
          };
        })
        .filter(({ title, pipeline }) =>
          `${title} ${pipeline?.metadata.description ?? ''}`
            .toLocaleLowerCase(locale)
            .includes(query.trim().toLocaleLowerCase(locale)),
        )
        .sort((a, b) =>
          sort === 'name'
            ? a.title.localeCompare(b.title, locale)
            : b.workspace.updatedAt.localeCompare(a.workspace.updatedAt),
        ),
    [workspaces, query, sort, locale, t],
  );
  return (
    <section className="page pipelines-page">
      <div className="page-heading">
        <div>
          <h1>{t('navigation.pipelines')}</h1>
          <p>{t('pipelines.intro')}</p>
        </div>
        <div className="heading-actions">
          <button className="button" disabled={busy} onClick={onImport}>
            <FolderOpen size={15} />
            {t('common.openPackage')}
          </button>
          <button className="button primary" onClick={onCreate}>
            <Plus size={15} />
            {t('shell.newPipeline')}
          </button>
        </div>
      </div>
      <div className="pipelines-toolbar">
        <label className="pipeline-search">
          <Search size={16} />
          <input
            aria-label={t('pipelines.search')}
            placeholder={t('pipelines.search')}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </label>
        <span className="muted" role="status">
          {t('pipelines.count', { count: pipelines.length })}
        </span>
        <select
          aria-label={t('pipelines.sort')}
          value={sort}
          onChange={(e) => setSort(e.target.value)}
        >
          <option value="updated">{t('pipelines.updatedFirst')}</option>
          <option value="name">{t('pipelines.byName')}</option>
        </select>
      </div>
      {pipelines.length ? (
        <div className="pipeline-cards">
          {pipelines.map(({ workspace, pipeline, title }) => {
            const nodes = Object.values(pipeline?.spec.nodes ?? {});
            return (
              <button
                className="pipeline-card"
                key={workspace.id}
                aria-label={t('pipelines.open', { name: title })}
                onClick={() => onOpen(workspace.id)}
              >
                <div className="pipeline-card-top">
                  <span className="node-icon tint-green">
                    <Workflow size={20} />
                  </span>
                  <span className="pipeline-draft-state">
                    {t(
                      !pipeline
                        ? 'pipelines.invalid'
                        : workspace.source === workspace.savedSource
                          ? 'pipelines.saved'
                          : 'pipelines.changed',
                    )}
                  </span>
                  <ArrowRight size={17} />
                </div>
                <h2>{title}</h2>
                <p>{pipeline?.metadata.description || t('pipelines.noDescription')}</p>
                <div
                  className="pipeline-preview"
                  aria-label={t('pipelines.nodes', { count: nodes.length })}
                >
                  {nodes.slice(0, 5).map((node, index) => (
                    <span className="pipeline-preview-step" key={index}>
                      <span
                        title={t(nodeMeta[node.type]?.label ?? 'editor.pipeline')}
                        className={`node-icon tint-${nodeMeta[node.type]?.color ?? 'green'}`}
                      >
                        <NodeIcon kind={node.type} size={15} />
                      </span>
                    </span>
                  ))}
                  {nodes.length > 5 ? <small>+{nodes.length - 5}</small> : null}
                </div>
                <div className="pipeline-card-footer">
                  <span>
                    {pipeline
                      ? t('pipelines.nodes', { count: nodes.length })
                      : t('pipelines.checkSource')}
                  </span>
                  <span>{time(workspace.updatedAt, locale)}</span>
                </div>
              </button>
            );
          })}
        </div>
      ) : (
        <Empty
          icon={<Workflow size={28} />}
          title={t(workspaces.length ? 'pipelines.noMatches' : 'pipelines.empty')}
          action={
            <button className="button" onClick={workspaces.length ? () => setQuery('') : onCreate}>
              {t(workspaces.length ? 'pipelines.clearSearch' : 'shell.newPipeline')}
            </button>
          }
        >
          {t(workspaces.length ? 'pipelines.trySearch' : 'pipelines.emptyHint')}
        </Empty>
      )}
    </section>
  );
}

export function TemplateLibrary({ onSelect }: { onSelect: (id: string) => void }) {
  const { t } = useI18n();
  const [category, setCategory] = useState<Example['category']>('starter');
  return (
    <div className="template-library">
      <div className="template-categories underline-tabs" aria-label={t('library.categories')}>
        {(['starter', 'block', 'demo'] as const).map((value) => (
          <button
            key={value}
            className={category === value ? 'active' : ''}
            aria-pressed={category === value}
            onClick={() => setCategory(value)}
          >
            {t(`library.category.${value}`)}
          </button>
        ))}
      </div>
      <p className="template-category-hint">{t(`library.hint.${category}`)}</p>
      <div className={`library-grid ${category === 'starter' ? 'starter-grid' : ''}`}>
        {examples
          .filter((example) => example.category === category)
          .map((example) => (
            <button key={example.id} className="template-card" onClick={() => onSelect(example.id)}>
              <span className={`node-icon tint-${category === 'starter' ? 'green' : 'violet'}`}>
                <NodeIcon
                  kind={
                    (
                      {
                        hello: 'llm',
                        'research-dossier': 'agent',
                        'tic-tac-toe': 'code',
                        publication: 'human',
                        subpipeline: 'pipeline',
                        'artifact-mount': 'code',
                        local: 'llm',
                      } as Record<string, NodeKind>
                    )[example.id] ?? (example.id as NodeKind)
                  }
                  size={21}
                />
              </span>
              <h3>{t(example.titleKey)}</h3>
              <p>{t(example.descriptionKey)}</p>
              {example.resultKey ? (
                <div className="template-result">
                  {t('library.result')}: {t(example.resultKey)}
                </div>
              ) : null}
              <div className="template-requirements">
                <span>{t('library.requires')}:</span>
                {example.requirements.length ? (
                  example.requirements.map((requirement) => (
                    <span className="requirement" key={requirement}>
                      {t(`library.requirement.${requirement}`)}
                    </span>
                  ))
                ) : (
                  <span>{t('library.noResources')}</span>
                )}
              </div>
              <span className="template-kind">
                {t(example.kindKey)}
                <ArrowRight size={14} />
              </span>
            </button>
          ))}
      </div>
    </div>
  );
}
