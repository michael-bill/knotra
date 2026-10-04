import { useEffect, useRef, type ReactNode } from 'react';
import {
  X,
  Workflow,
  Sparkles,
  Bot,
  Code2,
  Wrench,
  GitBranch,
  UserRound,
  Layers,
  Repeat2,
  Boxes,
} from 'lucide-react';
import type { NodeKind, NodeStatus, RunStatus } from '../lib/types';
import { localeTag, translate, useI18n, type Locale, type MessageKey } from '../lib/i18n';

export const nodeMeta = {
  llm: {
    label: 'editor.languageModel',
    icon: Sparkles,
    color: 'violet',
    detail: 'editor.oneStructuredModelResponse',
  },
  agent: {
    label: 'editor.aiAgent',
    icon: Bot,
    color: 'green',
    detail: 'editor.autonomousWorkWithinLimits',
  },
  code: {
    label: 'editor.code',
    icon: Code2,
    color: 'blue',
    detail: 'editor.aCommandInAnIsolatedSandbox',
  },
  tool: {
    label: 'editor.mcpTool',
    icon: Wrench,
    color: 'orange',
    detail: 'editor.anExplicitMcpToolCall',
  },
  switch: {
    label: 'editor.switch',
    icon: GitBranch,
    color: 'pink',
    detail: 'editor.chooseARouteWithCel',
  },
  human: {
    label: 'execution.humanReview',
    icon: UserRound,
    color: 'yellow',
    detail: 'editor.aSavedRequestForAPerson',
  },
  foreach: {
    label: 'editor.forEach',
    icon: Layers,
    color: 'blue',
    detail: 'editor.anIsolatedGraphForEveryItem',
  },
  loop: {
    label: 'editor.loop',
    icon: Repeat2,
    color: 'orange',
    detail: 'editor.repeatAGraphWithBoundedState',
  },
  pipeline: {
    label: 'editor.pipeline',
    icon: Boxes,
    color: 'green',
    detail: 'editor.callAReusablePipelinePackage',
  },
} as const satisfies Record<
  NodeKind,
  { label: MessageKey; detail: MessageKey; icon: typeof Workflow; color: string }
>;

export function NodeIcon({ kind, size = 18 }: { kind: NodeKind; size?: number }) {
  const Icon = nodeMeta[kind]?.icon ?? Workflow;
  return <Icon size={size} />;
}

export function Status({ status }: { status: RunStatus | NodeStatus }) {
  const { t } = useI18n();
  const names: Record<RunStatus | NodeStatus, MessageKey> = {
    ready: 'shell.queued',
    retry_wait: 'shell.retryWait',
    waiting_resolution: 'shell.needsResolution',
    waiting_human: 'execution.needsReview',
    succeeded: 'execution.completed',
    running: 'execution.running',
    pending: 'shell.pending',
    failed: 'execution.failed',
    cancelled: 'execution.cancelled',
    skipped: 'shell.skipped',
  };
  return (
    <span className={`status status-${status}`}>
      <span />
      {t(names[status])}
    </span>
  );
}

export function Empty({
  icon,
  title,
  children,
  action,
}: {
  icon: ReactNode;
  title: string;
  children: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="empty">
      <div className="empty-icon">{icon}</div>
      <h2>{title}</h2>
      <p>{children}</p>
      {action}
    </div>
  );
}

export function Modal({
  title,
  subtitle,
  children,
  onClose,
  wide = false,
}: {
  title: string;
  subtitle?: string;
  children: ReactNode;
  onClose: () => void;
  wide?: boolean;
}) {
  const { t } = useI18n();
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const previous = document.activeElement as HTMLElement;
    const panel = ref.current!;
    const focusables = () => [
      ...panel.querySelectorAll<HTMLElement>(
        'button:not(:disabled), input, textarea, select, [tabindex="0"]',
      ),
    ];
    if (!panel.contains(document.activeElement)) (focusables()[0] ?? panel).focus();
    function key(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose();
      if (e.key === 'Tab') {
        const items = focusables();
        const first = items[0];
        const last = items.at(-1);
        if (e.shiftKey && document.activeElement === first) {
          e.preventDefault();
          last?.focus();
        } else if (!e.shiftKey && document.activeElement === last) {
          e.preventDefault();
          first?.focus();
        }
      }
    }
    document.addEventListener('keydown', key);
    return () => {
      document.removeEventListener('keydown', key);
      previous?.focus();
    };
  }, [onClose]);
  return (
    <div
      className="modal-backdrop"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-labelledby="modal-title"
        tabIndex={-1}
        className={`modal ${wide ? 'modal-wide' : ''}`}
      >
        <header>
          <div>
            <h2 id="modal-title">{title}</h2>
            {subtitle ? <p>{subtitle}</p> : null}
          </div>
          <button className="icon-button" aria-label={t('common.closeDialog')} onClick={onClose}>
            <X size={20} />
          </button>
        </header>
        {children}
      </div>
    </div>
  );
}

export function time(value: string, locale: Locale = 'en'): string {
  return new Date(value).toLocaleString(localeTag(locale), {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
}

export function size(bytes: number, locale: Locale = 'en'): string {
  if (bytes < 1024) return `${bytes} ${translate('units.bytes', locale)}`;
  const amount = new Intl.NumberFormat(localeTag(locale), {
    useGrouping: false,
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  }).format(bytes / 1024);
  return `${amount} ${translate('units.kilobytes', locale)}`;
}
