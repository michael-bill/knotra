import { useEffect, useRef, type ReactNode } from 'react';
import { X, Workflow, Sparkles, Bot, Code2, Wrench, GitBranch, UserRound, Layers, Repeat2, Boxes } from 'lucide-react';
import type { NodeKind, NodeStatus, RunStatus } from '../lib/types';
export const nodeMeta = {
  llm: { label: 'Language model', icon: Sparkles, color: 'violet', detail: 'One structured model response' },
  agent: { label: 'AI agent', icon: Bot, color: 'green', detail: 'Autonomous work within limits' },
  code: { label: 'Code', icon: Code2, color: 'blue', detail: 'A command in an isolated sandbox' },
  tool: { label: 'MCP tool', icon: Wrench, color: 'orange', detail: 'An explicit MCP tool call' },
  switch: { label: 'Switch', icon: GitBranch, color: 'pink', detail: 'Choose a route with CEL' },
  human: { label: 'Human review', icon: UserRound, color: 'yellow', detail: 'A saved request for a person' },
  foreach: { label: 'For each', icon: Layers, color: 'blue', detail: 'An isolated graph for every item' },
  loop: { label: 'Loop', icon: Repeat2, color: 'orange', detail: 'Repeat a graph with bounded state' },
  pipeline: { label: 'Pipeline', icon: Boxes, color: 'green', detail: 'Call a reusable pipeline package' },
} as const;
export function NodeIcon({ kind, size = 18 }: { kind: NodeKind; size?: number }) { const Icon = nodeMeta[kind]?.icon ?? Workflow; return <Icon size={size} />; }
export function Status({ status }: { status: RunStatus | NodeStatus }) {
  const names: Record<string, string> = { ready: 'Queued', retry_wait: 'Retry wait', waiting_resolution: 'Needs resolution', waiting_human: 'Needs review', succeeded: 'Completed', running: 'Running', pending: 'Pending', failed: 'Failed', cancelled: 'Cancelled', skipped: 'Skipped' };
  return <span className={`status status-${status}`}><span />{names[status]}</span>;
}
export function Empty({ icon, title, children, action }: { icon: ReactNode; title: string; children: ReactNode; action?: ReactNode }) {
  return <div className="empty"><div className="empty-icon">{icon}</div><h2>{title}</h2><p>{children}</p>{action}</div>;
}
export function Modal({ title, subtitle, children, onClose, wide = false }: { title: string; subtitle?: string; children: ReactNode; onClose: () => void; wide?: boolean }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const previous = document.activeElement as HTMLElement;
    const panel = ref.current!;
    const focusables = () => [...panel.querySelectorAll<HTMLElement>('button:not(:disabled), input, textarea, select, [tabindex="0"]')];
    if (!panel.contains(document.activeElement)) (focusables()[0] ?? panel).focus();
    function key(e: KeyboardEvent) { if (e.key === 'Escape') onClose(); if (e.key === 'Tab') { const items = focusables(); const first = items[0]; const last = items.at(-1); if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last?.focus(); } else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first?.focus(); } } }
    document.addEventListener('keydown', key); return () => { document.removeEventListener('keydown', key); previous?.focus(); };
  }, [onClose]);
  return <div className="modal-backdrop" onMouseDown={e => { if (e.target === e.currentTarget) onClose(); }}><div ref={ref} role="dialog" aria-modal="true" aria-labelledby="modal-title" tabIndex={-1} className={`modal ${wide ? 'modal-wide' : ''}`}><header><div><h2 id="modal-title">{title}</h2>{subtitle ? <p>{subtitle}</p> : null}</div><button className="icon-button" aria-label="Close dialog" onClick={onClose}><X size={20} /></button></header>{children}</div></div>;
}
export function time(value: string): string { return new Date(value).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }); }
export function size(bytes: number): string { return bytes < 1024 ? `${bytes} B` : `${(bytes / 1024).toFixed(1)} KB`; }
