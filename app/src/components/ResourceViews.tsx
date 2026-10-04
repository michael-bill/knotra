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

export function ConnectionsView({
  pipeline,
  onSource,
  engine,
}: {
  engine: EngineController;
  pipeline?: Pipeline;
  onSource: (kind: 'models' | 'mcp' | 'sandboxes' | 'secrets') => void;
}) {
  const [tab, setTab] = useState<'models' | 'mcp' | 'sandboxes' | 'secrets'>('models');
  const meta = {
    models: { title: 'Models', icon: Bot },
    mcp: { title: 'MCP tools', icon: Wrench },
    sandboxes: { title: 'Sandboxes', icon: Box },
    secrets: { title: 'Secret references', icon: KeyRound },
  };
  const resources = Object.entries(pipeline?.spec[tab] ?? {});
  const Icon = meta[tab].icon;
  return (
    <section className="page">
      <header className="page-heading">
        <div>
          <div className="breadcrumb">
            Workspace
            <ChevronRight size={13} />
            Resources
          </div>
          <h1>Models & tools</h1>
          <p>
            Logical resources declared by{' '}
            {pipeline?.metadata.title ?? pipeline?.metadata.name ?? 'your selected pipeline'}.
          </p>
        </div>
        <button className="button" onClick={() => onSource(tab)}>
          Edit pipeline resources
          <ArrowUpRight size={15} />
        </button>
      </header>
      <div className="notice">
        <ShieldCheck size={18} />
        <div>
          <strong>Resolved by your engine profile</strong>
          <p>
            A pipeline declares aliases. The trusted engine supplies credentials, applies
            permissions, and checks capabilities during run admission. Secret values stay on the
            engine.
          </p>
        </div>
      </div>
      {engine.info ? (
        <div className="settings-card">
          <h2>Engine resource catalog</h2>
          <p className="small muted">
            Connections configured in the engine profiles. Availability reflects provider and
            credential configuration; run admission checks model capabilities and dependencies.
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
                <span className="type-label">Declared</span>
              </div>
              <h2>{id}</h2>
              <div className="detail-field">
                <span>
                  {tab === 'sandboxes'
                    ? 'Profile'
                    : tab === 'secrets'
                      ? 'Secret reference'
                      : 'Connection'}
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
                Checked during run admission
              </footer>
            </article>
          ))}
        </div>
      ) : (
        <Empty icon={<Icon size={30} />} title={`No ${meta[tab].title.toLowerCase()} declared`}>
          Add resource aliases in the pipeline YAML when a node needs them.
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
  onSaveUrl: (url: string) => void;
  onBackup: () => void;
  onRestore: () => void;
  onNotify: (message: string) => void;
}) {
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
      onNotify('Engine address saved. Connect to check its protocol and availability.');
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Enter a valid engine URL.');
    }
  }
  return (
    <section className="page settings-page">
      <header className="page-heading">
        <div>
          <div className="breadcrumb">
            Workspace
            <ChevronRight size={13} />
            Preferences
          </div>
          <h1>Workspace settings</h1>
          <p>A local desktop client for processes under your control.</p>
        </div>
        <span className="type-label">v0.1.0</span>
      </header>
      <div className="settings-card">
        <div className="settings-section-heading">
          <Sun size={21} />
          <div>
            <h2>Appearance</h2>
            <p>Choose the palette for your workspace and editor.</p>
          </div>
        </div>
        <div className="theme-options" role="radiogroup" aria-label="Color theme">
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
                      `[aria-label="${next === 'dark' ? 'Dark' : 'Light'}"]`,
                    ) as HTMLButtonElement
                  )?.focus();
                }
              }}
              aria-label={option === 'dark' ? 'Dark' : 'Light'}
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
                  {option === 'dark' ? 'Dark' : 'Light'}
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
            <h2>Engine</h2>
            <p>Connect to your Knotra engine to run workflows and review their results.</p>
          </div>
          <span className={`status status-${engine.info ? 'succeeded' : 'pending'}`}>
            <span />
            {engine.info ? (engine.error ? 'Unavailable' : 'Connected') : 'Disconnected'}
          </span>
        </div>
        <label className="field">
          Engine base URL
          <div className="inline">
            <input
              aria-label="Engine base URL"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
            />
            <button className="button" onClick={save}>
              Save address
            </button>
          </div>
        </label>
        {error ? (
          <p className="form-error" role="alert">
            {error}
          </p>
        ) : null}
        <label className="field">
          Access token (optional for loopback)
          <input
            type="password"
            aria-label="Engine access token"
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
            {engine.connecting ? 'Connecting…' : 'Connect engine'}
          </button>
          {engine.info ? (
            <button
              className="button"
              onClick={() => {
                void engine.disconnect().catch((error) => onNotify(String(error)));
              }}
            >
              Disconnect
            </button>
          ) : null}
        </div>
        {engine.error ? (
          <p className="form-error" role="alert">
            {engine.error}
          </p>
        ) : null}
        {engine.info ? (
          <div className="detail-field">
            <span>Engine identity · protocol</span>
            <code>
              {engine.info.engineId} · {engine.info.protocol}
            </code>
            <small>
              {engine.profiles.length} profiles · {engine.resources.length} resources
            </small>
          </div>
        ) : null}
        <div className="notice small">
          Start your local Knotra engine and connect using its address. Running workflows continue
          on the engine when you close the app. Remote engines require HTTPS and an access token;
          tokens stay in memory until you disconnect or close the app.
        </div>
      </div>
      <div className="settings-card">
        <div className="settings-section-heading">
          <Monitor size={21} />
          <div>
            <h2>Workspace</h2>
            <p>Pipeline drafts, settings and demo history stay on this device.</p>
          </div>
        </div>
        <div className="setting-row">
          <div>
            <strong>Back up your workspace</strong>
            <p>Download drafts, supporting files and demo run history as JSON.</p>
          </div>
          <button className="button" onClick={onBackup}>
            Export backup
          </button>
        </div>
        <div className="setting-row">
          <div>
            <strong>Restore a workspace backup</strong>
            <p>Review a JSON backup before replacing local drafts and demo history.</p>
          </div>
          <button className="button" onClick={onRestore}>
            Choose backup
          </button>
        </div>
        <div className="setting-row">
          <div>
            <strong>Application runtime</strong>
            <p>
              {desktop
                ? 'Tauri desktop · native package dialogs'
                : 'Browser preview · YAML and ZIP import/export'}
            </p>
          </div>
          <span className="type-label">{desktop ? 'Desktop' : 'Preview'}</span>
        </div>
      </div>
      <div className="about-card">
        <img src="/knotra.svg" alt="" />
        <div>
          <h2>Knotra</h2>
          <p>Make the process explicit.</p>
        </div>
        <button
          className="text-button"
          onClick={() => {
            void openProjectDocs().catch((error) => onNotify(String(error)));
          }}
        >
          Project documentation
          <ExternalLink size={14} />
        </button>
      </div>
    </section>
  );
}
