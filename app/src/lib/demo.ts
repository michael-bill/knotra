import { RESEARCH_SOURCE } from './examples';
import { sha256, textBytes } from './bytes';
import { parsePipeline, validateResponse } from './validation';
import type { DemoRun, Json, RunEvent, Workspace } from './types';

export const demoNodeIds = ['discover', 'research', 'draft', 'review', 'publish'];

export function demoAvailable(workspace: Workspace): boolean {
  return workspace.source === RESEARCH_SOURCE && workspace.files.length === 0;
}

function event(message: string, node?: string): RunEvent {
  return { id: crypto.randomUUID(), at: new Date().toISOString(), message, node };
}

export function createDemoRun(workspace: Workspace, topic: string): DemoRun {
  if (!demoAvailable(workspace))
    throw new Error(
      'The guided demo requires an unchanged Research brief template. Run edited workflows on a connected engine.',
    );
  if (!topic.trim()) throw new Error('Enter a research topic.');
  const now = new Date().toISOString();
  return {
    id: crypto.randomUUID(),
    workspaceId: workspace.id,
    title: 'Research brief',
    topic: topic.trim(),
    createdAt: now,
    updatedAt: now,
    status: 'running',
    source: workspace.source,
    files: workspace.files.map((f) => ({ ...f })),
    inputs: { topic: topic.trim() },
    nodes: {
      discover: 'running',
      research: 'pending',
      draft: 'pending',
      review: 'pending',
      publish: 'pending',
    },
    events: [
      event('Demo started. Outputs are sample data; no models, tools or commands are called.'),
      event('Gathering sample source material.', 'discover'),
    ],
    outputs: {},
    artifacts: [],
  };
}

export function briefFor(run: DemoRun): string {
  return `# ${run.topic}\n\nThis is a demonstration brief. It contains sample content, not research findings.\n\n## A process you can understand\n\nA workflow makes the transfer of data explicit. Each node declares its inputs, outputs and dependencies.\n\n- Validate contracts before execution.\n- Keep a fixed snapshot for each run.\n- Give agents explicit tools and bounded resources.\n- Preserve history when a client disconnects.\n- Ask a person to review the final result.\n\n## Questions for real research\n\nWhich sources support the conclusions? What remains uncertain? What would change the recommendation?`;
}

export function advanceDemo(run: DemoRun): DemoRun {
  if (run.status !== 'running') return run;
  const active = demoNodeIds.find((id) => run.nodes[id] === 'running');
  if (!active || active === 'publish') return run;
  const index = demoNodeIds.indexOf(active);
  const next = demoNodeIds[index + 1];
  const waiting = next === 'review';
  return {
    ...run,
    updatedAt: new Date().toISOString(),
    nodes: { ...run.nodes, [active]: 'succeeded', [next]: waiting ? 'waiting_human' : 'running' },
    status: waiting ? 'waiting_human' : 'running',
    requestId: waiting ? crypto.randomUUID() : run.requestId,
    events: [
      ...run.events,
      event('Sample output saved.', active),
      event(
        waiting
          ? 'Waiting for a response to the saved review request.'
          : 'Preparing sample output.',
        next,
      ),
    ],
  };
}

export function respondDemo(
  run: DemoRun,
  requestId: string,
  response: Record<string, Json>,
): DemoRun {
  if (run.status !== 'waiting_human' || requestId !== run.requestId)
    throw new Error('This review request is no longer open.');
  const pipeline = parsePipeline(run.source);
  if (!pipeline) throw new Error('The run snapshot is invalid.');
  const errors = validateResponse(pipeline, 'review', response);
  if (errors.length) throw new Error(errors.join('\n'));
  return {
    ...run,
    response,
    status: 'running',
    updatedAt: new Date().toISOString(),
    nodes: { ...run.nodes, review: 'succeeded', publish: 'running' },
    events: [
      ...run.events,
      event('Response accepted for this review request.', 'review'),
      event('Preparing a sample Markdown artifact.', 'publish'),
    ],
  };
}

export async function finishDemo(run: DemoRun): Promise<DemoRun> {
  if (run.status !== 'running' || run.nodes.publish !== 'running' || !run.response) return run;
  const content = `${briefFor(run)}\n\n## Reviewer feedback\n\n${String(run.response.feedback)}\n`;
  const bytes = textBytes(content);
  const hash = await sha256(bytes);
  const id = crypto.randomUUID();
  return {
    ...run,
    status: 'succeeded',
    updatedAt: new Date().toISOString(),
    nodes: { ...run.nodes, publish: 'succeeded' },
    outputs: {
      document: { artifactId: id, mediaType: 'text/markdown', sha256: hash, size: bytes.length },
    },
    artifacts: [
      {
        id,
        name: 'brief.md',
        mediaType: 'text/markdown',
        content,
        sha256: hash,
        size: bytes.length,
      },
    ],
    events: [
      ...run.events,
      event('Sample artifact collected and hashed.', 'publish'),
      event('Demo completed. No external actions were performed.'),
    ],
  };
}

export function cancelDemo(run: DemoRun): DemoRun {
  if (!['running', 'waiting_human'].includes(run.status)) return run;
  return {
    ...run,
    status: 'cancelled',
    updatedAt: new Date().toISOString(),
    nodes: Object.fromEntries(
      Object.entries(run.nodes).map(([id, state]) => [
        id,
        ['pending', 'running', 'waiting_human'].includes(state) ? 'cancelled' : state,
      ]),
    ),
    events: [...run.events, event('Demo cancelled. No further steps will run.')],
  };
}
