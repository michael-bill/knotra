import type { MessageKey } from './i18n';

// Display labels are separate from the engine protocol and local view state.
export const executionCommandLabels: Record<string, MessageKey> = {
  validate: 'execution.validate',
  publish: 'execution.publish',
  start: 'execution.start',
  cancel: 'execution.cancel',
  resume: 'execution.resume',
  respond: 'execution.respond',
  resolve: 'execution.resolve',
  upload: 'execution.upload',
};

export const executionStatusLabels: Record<string, MessageKey> = {
  all: 'execution.allStatuses',
  pending: 'execution.pending',
  ready: 'execution.ready',
  running: 'execution.running2',
  retry_wait: 'execution.retryWait',
  waiting_human: 'execution.waitingHuman',
  waiting_resolution: 'execution.waitingResolution',
  succeeded: 'execution.succeeded',
  failed: 'execution.failed2',
  cancelled: 'execution.cancelled2',
};

export const demoStatusLabels: Record<string, MessageKey> = {
  all: 'execution.allStatuses',
  running: 'execution.running',
  waiting_human: 'execution.needsReview',
  succeeded: 'execution.completed',
  cancelled: 'execution.cancelled',
};

export const executionActionLabels: Record<string, MessageKey> = {
  cancel: 'execution.requestCancellation',
  resume: 'execution.resumeSavedAttempt',
};

export const executionTabLabels: Record<string, MessageKey> = {
  instances: 'execution.instances',
  timeline: 'execution.timeline',
  inputs: 'execution.inputs',
  outputs: 'execution.outputs',
  snapshot: 'execution.snapshot',
};

export const executionConnectionLabels: Record<string, MessageKey> = {
  connected: 'execution.connected',
  reconnecting: 'execution.reconnecting',
};
