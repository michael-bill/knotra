import type { EngineController } from '../lib/engine/useEngine';
import { useState } from 'react';
import {
  Bot,
  Wrench,
  Box,
  KeyRound,
  ChevronRight,
  ArrowUpRight,
  ShieldCheck,
  Monitor,
  Globe,
  ExternalLink,
  Moon,
  Sun,
  Check,
} from 'lucide-react';
import type { Theme } from '../lib/theme';
import type { Pipeline } from '../lib/types';
import { record } from '../lib/types';
import { Empty } from './ui';
import { openProjectDocs } from '../lib/native';
import { useI18n, type Locale, type MessageKey } from '../lib/i18n';

export function ConnectionsView({
  pipeline,
  onSource,
  engine,
}: {
  engine: EngineController;
  pipeline?: Pipeline;
  onSource: (kind: 'models' | 'mcp' | 'sandboxes' | 'secrets') => void;
}) {
  const { t } = useI18n();
  const [tab, setTab] = useState<'models' | 'mcp' | 'sandboxes' | 'secrets'>('models');
  const meta = {
    models: { title: t('resources.models'), empty: 'resources.noModelsDeclared', icon: Bot },
    mcp: { title: t('resources.mcpTools'), empty: 'resources.noMcpToolsDeclared', icon: Wrench },
    sandboxes: {
      title: t('resources.sandboxes'),
      empty: 'resources.noSandboxesDeclared',
      icon: Box,
    },
    secrets: {
      title: t('resources.secretReferences'),
      empty: 'resources.noSecretReferencesDeclared',
      icon: KeyRound,
    },
  } satisfies Record<typeof tab, { title: string; empty: MessageKey; icon: typeof Bot }>;
  const resources = Object.entries(pipeline?.spec[tab] ?? {});
  const Icon = meta[tab].icon;
  return (
    <section className="page">
      <header className="page-heading">
        <div>
          <div className="breadcrumb">
            {t('resources.workspace')}
            <ChevronRight size={13} />
            {t('navigation.resources')}
          </div>
          <h1>{t('resources.modelsTools')}</h1>
          <p>
            {t('resources.logicalResourcesDeclaredByName', {
              name:
                pipeline?.metadata.title ??
                pipeline?.metadata.name ??
                t('resources.yourSelectedPipeline'),
            })}
          </p>
        </div>
        <button className="button" onClick={() => onSource(tab)}>
          {t('resources.editPipelineResources')}
          <ArrowUpRight size={15} />
        </button>
      </header>
      <div className="notice">
        <ShieldCheck size={18} />
        <div>
          <strong>{t('resources.resolvedByYourEngineProfile')}</strong>
          <p>{t('resources.aPipelineDeclaresAliasesTheTrustedEngineSupplies')}</p>
        </div>
      </div>
      {engine.info ? (
        <div className="settings-card">
          <h2>{t('resources.engineResourceCatalog')}</h2>
          <p className="small muted">
            {t('resources.connectionsConfiguredInTheEngineProfilesAvailabilityReflects')}
          </p>
          <div className="engine-resource-list">
            {engine.resources.map((resource) => (
              <div key={JSON.stringify([resource.kind, resource.title, resource.id])}>
                <strong>{resource.title}</strong>
                <code>{resource.id}</code>
                <span>
                  {resource.kind} · {resource.status}
                </span>
                <small>{resource.capabilities.join(', ')}</small>
              </div>
            ))}
          </div>
        </div>
      ) : null}
      <div className="underline-tabs resource-tabs">
        {Object.entries(meta).map(([key, m]) => (
          <button
            key={key}
            onClick={() => setTab(key as typeof tab)}
            className={key === tab ? 'active' : ''}
          >
            <m.icon size={16} />
            {m.title}
            <span className="counter">
              {Object.keys(pipeline?.spec[key as typeof tab] ?? {}).length}
            </span>
          </button>
        ))}
      </div>
      {resources.length ? (
        <div className="resource-grid">
          {resources.map(([id, resource]) => (
            <article className="resource-card" key={id}>
              <div className="inline spread">
                <span className="resource-icon">
                  <Icon size={22} />
                </span>
                <span className="type-label">{t('resources.declared')}</span>
              </div>
              <h2>{id}</h2>
              <div className="detail-field">
                <span>
                  {t(
                    tab === 'sandboxes'
                      ? 'resources.profile'
                      : tab === 'secrets'
                        ? 'resources.secretReference'
                        : 'resources.connection',
                  )}
                </span>
                <code>
                  {String(
                    record(resource).connection ?? record(resource).profile ?? record(resource).ref,
                  )}
                </code>
              </div>
              {Array.isArray(record(resource).requires) ? (
                <div className="capability-list">
                  {(record(resource).requires as string[]).map((capability) => (
                    <span key={capability}>{capability}</span>
                  ))}
                </div>
              ) : null}
              <footer>
                <span className="dot amber" />
                {t('resources.checkedDuringRunAdmission')}
              </footer>
            </article>
          ))}
        </div>
      ) : (
        <Empty icon={<Icon size={30} />} title={t(meta[tab].empty)}>
          {t('resources.addResourceAliasesInThePipelineYamlWhen')}
        </Empty>
      )}
    </section>
  );
}

export function SettingsView({
  engine,
  engineUrl,
  desktop,
  theme,
  onTheme,
  language,
  onLanguage,
  onSaveUrl,
  onBackup,
  onRestore,
  onNotify,
}: {
  engine: EngineController;
  engineUrl: string;
  desktop: boolean;
  theme: Theme;
  onTheme: (theme: Theme) => void;
  language: Locale;
  onLanguage: (locale: Locale) => void;
  onSaveUrl: (url: string) => void;
  onBackup: () => void;
  onRestore: () => void;
  onNotify: (message: string) => void;
}) {
  const { t, message } = useI18n();
  const [url, setUrl] = useState(engineUrl);
  const [error, setError] = useState('');
  const [token, setToken] = useState('');
  function save() {
    try {
      const parsed = new URL(url);
      if (
        !['http:', 'https:'].includes(parsed.protocol) ||
        parsed.username ||
        parsed.password ||
        parsed.search ||
        parsed.hash
      )
        throw new Error('Use an HTTP(S) base URL without credentials, query or fragment.');
      onSaveUrl(parsed.toString().replace(/\/$/, ''));
      setError('');
      onNotify('resources.engineAddressSavedConnectToCheckItsProtocol');
    } catch (e) {
      setError(
        e instanceof TypeError || !(e instanceof Error)
          ? 'resources.enterAValidEngineUrl'
          : e.message,
      );
    }
  }
  return (
    <section className="page settings-page">
      <header className="page-heading">
        <div>
          <div className="breadcrumb">
            {t('resources.workspace')}
            <ChevronRight size={13} />
            {t('resources.preferences')}
          </div>
          <h1>{t('resources.workspaceSettings')}</h1>
          <p>{t('resources.aLocalDesktopClientForProcessesUnderYour')}</p>
        </div>
        <span className="type-label">v0.1.0</span>
      </header>
      <div className="settings-card">
        <div className="settings-section-heading">
          <Sun size={21} />
          <div>
            <h2>{t('settings.appearance')}</h2>
            <p>{t('resources.chooseThePaletteForYourWorkspaceAndEditor')}</p>
          </div>
        </div>
        <div className="theme-options" role="radiogroup" aria-label={t('resources.colorTheme')}>
          {(['dark', 'light'] as const).map((option) => (
            <button
              key={option}
              type="button"
              role="radio"
              aria-checked={theme === option}
              tabIndex={theme === option ? 0 : -1}
              onKeyDown={(event) => {
                if (
                  ['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Home', 'End'].includes(
                    event.key,
                  )
                ) {
                  event.preventDefault();
                  const next =
                    event.key === 'Home'
                      ? 'dark'
                      : event.key === 'End'
                        ? 'light'
                        : option === 'dark'
                          ? 'light'
                          : 'dark';
                  onTheme(next);
                  (
                    event.currentTarget.parentElement?.querySelector(
                      `[aria-label="${t(next === 'dark' ? 'resources.dark' : 'resources.light')}"]`,
                    ) as HTMLButtonElement
                  )?.focus();
                }
              }}
              aria-label={t(option === 'dark' ? 'resources.dark' : 'resources.light')}
              className={`theme-option ${theme === option ? 'selected' : ''}`}
              onClick={() => onTheme(option)}
            >
              <span className={`theme-preview theme-preview-${option}`} aria-hidden="true">
                <i />
                <span>
                  <b />
                  <b />
                  <b />
                </span>
              </span>
              <span className="inline spread">
                <span className="inline">
                  {option === 'dark' ? <Moon size={15} /> : <Sun size={15} />}
                  {t(option === 'dark' ? 'resources.dark' : 'resources.light')}
                </span>
                {theme === option ? <Check size={15} /> : null}
              </span>
            </button>
          ))}
        </div>
      </div>
      <div className="settings-card">
        <div className="settings-section-heading">
          <Globe size={21} />
          <div>
            <h2>{t('settings.language')}</h2>
            <p>{t('resources.chooseTheLanguageForTheInterface')}</p>
          </div>
        </div>
        <label className="field">
          {t('settings.language')}
          <select
            aria-label={t('settings.language')}
            value={language}
            onChange={(event) => onLanguage(event.target.value as Locale)}
          >
            <option value="ru">Русский</option>
            <option value="en">English</option>
          </select>
        </label>
      </div>
      <div className="settings-card">
        <div className="settings-section-heading">
          <Globe size={21} />
          <div>
            <h2>{t('resources.engine')}</h2>
            <p>{t('resources.connectToYourKnotraEngineToRunWorkflows')}</p>
          </div>
          <span className={`status status-${engine.info ? 'succeeded' : 'pending'}`}>
            <span />
            {t(
              engine.info
                ? engine.error
                  ? 'resources.unavailable'
                  : 'resources.connected'
                : 'resources.disconnected',
            )}
          </span>
        </div>
        <label className="field">
          {t('resources.engineBaseUrl')}
          <div className="inline">
            <input
              aria-label={t('resources.engineBaseUrl')}
              value={url}
              onChange={(e) => setUrl(e.target.value)}
            />
            <button className="button" onClick={save}>
              {t('resources.saveAddress')}
            </button>
          </div>
        </label>
        {error ? (
          <p className="form-error" role="alert">
            {message(error)}
          </p>
        ) : null}
        <label className="field">
          {t('resources.accessTokenOptionalForLoopback')}
          <input
            type="password"
            aria-label={t('resources.engineAccessToken')}
            autoComplete="off"
            value={token}
            onChange={(event) => setToken(event.target.value)}
          />
        </label>
        <div className="inline">
          <button
            className="button primary"
            disabled={engine.connecting}
            onClick={() => {
              void engine.connect(url, token || undefined);
              setToken('');
            }}
          >
            {t(engine.connecting ? 'resources.connecting' : 'resources.connectEngine')}
          </button>
          {engine.info ? (
            <button
              className="button"
              onClick={() => {
                void engine.disconnect().catch((error) => onNotify(String(error)));
              }}
            >
              {t('resources.disconnect')}
            </button>
          ) : null}
        </div>
        {engine.error ? (
          <p className="form-error" role="alert">
            {t('resources.connectionError')}: {engine.error}
          </p>
        ) : null}
        {engine.info ? (
          <div className="detail-field">
            <span>{t('resources.engineIdentityProtocol')}</span>
            <code>
              {engine.info.engineId} · {engine.info.protocol}
            </code>
            <small>
              {t('resources.profilesProfilesResourcesResources', {
                profiles: engine.profiles.length,
                resources: engine.resources.length,
              })}
            </small>
          </div>
        ) : null}
        <div className="notice small">
          {t('resources.startYourLocalKnotraEngineAndConnectUsing')}
        </div>
      </div>
      <div className="settings-card">
        <div className="settings-section-heading">
          <Monitor size={21} />
          <div>
            <h2>{t('resources.workspace')}</h2>
            <p>{t('resources.pipelineDraftsSettingsAndDemoHistoryStayOn')}</p>
          </div>
        </div>
        <div className="setting-row">
          <div>
            <strong>{t('resources.backUpYourWorkspace')}</strong>
            <p>{t('resources.downloadDraftsSupportingFilesAndDemoRunHistory')}</p>
          </div>
          <button className="button" onClick={onBackup}>
            {t('resources.exportBackup')}
          </button>
        </div>
        <div className="setting-row">
          <div>
            <strong>{t('resources.restoreAWorkspaceBackup')}</strong>
            <p>{t('resources.reviewAJsonBackupBeforeReplacingLocalDrafts')}</p>
          </div>
          <button className="button" onClick={onRestore}>
            {t('resources.chooseBackup')}
          </button>
        </div>
        <div className="setting-row">
          <div>
            <strong>{t('resources.applicationRuntime')}</strong>
            <p>
              {t(
                desktop
                  ? 'resources.tauriDesktopNativePackageDialogs'
                  : 'resources.browserPreviewYamlAndZipImportExport',
              )}
            </p>
          </div>
          <span className="type-label">
            {t(desktop ? 'resources.desktop' : 'resources.preview')}
          </span>
        </div>
      </div>
      <div className="about-card">
        <img src="/knotra.svg" alt="" />
        <div>
          <h2>Knotra</h2>
          <p>{t('resources.makeTheProcessExplicit')}</p>
        </div>
        <button
          className="text-button"
          onClick={() => {
            void openProjectDocs().catch((error) => onNotify(String(error)));
          }}
        >
          {t('resources.projectDocumentation')}
          <ExternalLink size={14} />
        </button>
      </div>
    </section>
  );
}
