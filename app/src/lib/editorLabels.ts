import { portType } from './graph';
import type { MessageKey, useI18n } from './i18n';
import type { Diagnostic, NodeStatus, Port } from './types';

type Translate = ReturnType<typeof useI18n>['t'];

export const editorTypeKeys: Record<string, MessageKey> = {
  string: 'types.string',
  number: 'types.number',
  integer: 'types.integer',
  boolean: 'types.boolean',
  object: 'types.object',
  array: 'types.array',
  null: 'editor.null',
  JSON: 'types.json',
  enum: 'editor.enum',
  file: 'editor.file2',
  files: 'editor.files2',
};

export const editorStatusKeys: Record<NodeStatus, MessageKey> = {
  pending: 'execution.pending',
  ready: 'execution.ready',
  running: 'execution.running2',
  waiting_human: 'editor.waitingHuman',
  waiting_resolution: 'editor.waitingResolution',
  retry_wait: 'editor.retryWait',
  succeeded: 'execution.succeeded',
  failed: 'execution.failed2',
  cancelled: 'execution.cancelled2',
  skipped: 'editor.skipped',
};

export const editorDirectionKeys: Record<string, MessageKey> = {
  up: 'editor.up',
  down: 'editor.down',
  left: 'editor.left',
  right: 'editor.right',
};

export const editorLayerKeys: Record<Diagnostic['layer'], MessageKey> = {
  parse: 'editor.parse',
  structural: 'editor.structural',
  semantic: 'editor.semantic',
  package: 'editor.layerPackage',
  admission: 'editor.layerAdmission',
};

export function editorDataTypeLabel(type: string, t: Translate): string {
  return Object.hasOwn(editorTypeKeys, type) ? t(editorTypeKeys[type]) : type;
}

export function editorPortTypeLabel(port: Port, t: Translate): string {
  if (port.schemaRef !== undefined) return port.schemaRef;
  return portType(port)
    .split(' | ')
    .map((type) => editorDataTypeLabel(type, t))
    .join(' | ');
}
