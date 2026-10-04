import { describe, expect, it } from 'vitest';
import { decodeText } from '../src/lib/bytes';
import { enginePackage } from '../src/lib/engine/package';
import { examples, fromExample, initialWorkspaces, RESEARCH_SOURCE } from '../src/lib/examples';
import { graphOrder } from '../src/lib/graph';
import { translate } from '../src/lib/i18n';
import { parsePipeline, validateResponse } from '../src/lib/validation';

const starterIDs = ['hello', 'research-dossier', 'reviewed-research', 'tic-tac-toe', 'publication'];
const example = (id: string) => examples.find((candidate) => candidate.id === id)!;

describe('runnable starter packages', () => {
  it('seeds five scenarios while keeping the demo and technical blocks in the library', () => {
    expect(examples.filter((item) => item.category === 'starter').map((item) => item.id)).toEqual(
      starterIDs,
    );
    const initial = initialWorkspaces();
    expect(initial).toHaveLength(5);
    expect(initial.map((workspace) => parsePipeline(workspace.source)!.metadata.name)).toEqual([
      'hello-world',
      'research-dossier',
      'reviewed-research',
      'build-tic-tac-toe',
      'publication-workflow',
    ]);
    for (const id of starterIDs)
      expect(example(id).requirements).toEqual([
        'model',
        id === 'tic-tac-toe' ? 'nodeSandbox' : 'sandbox',
      ]);
    for (const id of [
      'local',
      'llm',
      'agent',
      'code',
      'tool',
      'switch',
      'human',
      'foreach',
      'loop',
      'subpipeline',
      'artifact-mount',
    ])
      expect(example(id).category).toBe('block');
    expect(example('research').category).toBe('demo');
    expect(fromExample(example('research'), 'ru').source).toBe(RESEARCH_SOURCE);
  });

  it('localizes only new workspace metadata and leaves all executable content unchanged', () => {
    for (const id of starterIDs) {
      const template = example(id);
      const original = parsePipeline(template.source)!;
      for (const locale of ['en', 'ru'] as const) {
        const workspace = fromExample(template, locale);
        const pipeline = parsePipeline(workspace.source)!;
        expect(pipeline.metadata.title).toBe(translate(template.titleKey, locale));
        expect(pipeline.metadata.title).not.toBe(template.titleKey);
        expect(pipeline.metadata.description).toBe(translate(template.descriptionKey, locale));
        expect(pipeline.metadata.name).toBe(original.metadata.name);
        expect(pipeline.spec).toEqual(original.spec);
        expect(workspace.savedSource).toBe(workspace.source);
      }
      expect(parsePipeline(template.source)).toEqual(original);
    }
    expect(parsePipeline(initialWorkspaces('ru')[0].source)!.metadata.title).toBe(
      translate('starter.hello.title', 'ru'),
    );
  });

  it('publishes all research evidence and scoring code while isolating edits between workspaces', () => {
    const template = example('research-dossier');
    const first = fromExample(template);
    const second = fromExample(template);
    const files = enginePackage(first).files;
    expect(files.map((file) => file.path).sort()).toEqual([
      'pipeline.yaml',
      'schemas/evidence.json',
      'schemas/weights.json',
      'scripts/compare.py',
      'scripts/package.py',
      'sources/cms.md',
      'sources/custom.md',
      'sources/hosted.md',
    ]);
    expect(decodeText(files.find((file) => file.path === 'sources/hosted.md')!.content)).toContain(
      'Fictional sample offer',
    );
    first.files[0].content = '';
    expect(second.files[0].content).toBe(template.files[0].content);
    expect(second.files[0].content).not.toBe('');
    const pipeline = parsePipeline(second.source)!;
    expect(graphOrder(pipeline.spec).order).toEqual([
      'evidence',
      'compare',
      'synthesize',
      'package',
    ]);
    expect(pipeline.spec.nodes.evidence.foreach).toMatchObject({
      over: 'sources',
      concurrency: 1,
      body: {
        nodes: { extract: { type: 'agent', tools: { inherit: false, sandbox: ['files.read'] } } },
      },
    });
    expect(pipeline.spec.nodes.compare.inputs!.evidence.bind).toEqual({
      from: 'nodes.evidence.outputs.evidence',
    });
    expect(pipeline.spec.nodes.synthesize.inputs!.comparison.bind).toEqual({
      from: 'nodes.compare.outputs.comparison',
    });
    expect(pipeline.spec.nodes.package.outputs!.bundle.collect).toEqual({
      path: 'dossier.zip',
      mediaType: 'application/zip',
    });
  });

  it('routes human feedback into a second model call and publishes that revised output', () => {
    const pipeline = parsePipeline(example('publication').source)!;
    const nodes = pipeline.spec.nodes;
    expect(graphOrder(pipeline.spec).order).toEqual([
      'draft',
      'editor',
      'approve',
      'revise',
      'package',
    ]);
    expect(nodes.revise.type).toBe('llm');
    expect(nodes.revise.inputs!.feedback.bind).toEqual({ from: 'nodes.approve.outputs.feedback' });
    expect(nodes.revise.inputs!.draft.bind).toEqual({ from: 'nodes.draft.outputs.text' });
    expect(nodes.package.inputs!.text.bind).toEqual({ from: 'nodes.revise.outputs.text' });
    expect(pipeline.spec.outputs.text.bind).toEqual({ from: 'nodes.revise.outputs.text' });
    expect(pipeline.spec.outputs.document.bind).toEqual({ from: 'nodes.package.outputs.document' });
    expect(
      validateResponse(pipeline, 'approve', {
        approved: true,
        feedback: 'Use two concise paragraphs.',
      }),
    ).toEqual([]);
    expect(
      validateResponse(pipeline, 'approve', { approved: false, feedback: 'Proceed.' }),
    ).not.toEqual([]);
  });
});
