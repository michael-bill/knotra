import { describe, expect, it } from 'vitest';
import { compatible, connectionError, sources } from '../src/lib/connections';
import { researchPipeline } from '../src/lib/examples';
import { nodePorts } from '../src/lib/graph';
import type { Graph } from '../src/lib/types';

describe('visual data bindings', () => {
  it('offers workflow inputs and named outputs, including derived child outputs', () => {
    const graph = structuredClone(researchPipeline.spec);
    expect(sources(graph).map(source => source.value)).toContain('inputs.topic');
    expect(sources(graph).map(source => source.value)).toContain('nodes.draft.outputs.brief');
    const child: Graph = { nodes: {}, outputs: { answer: { schema: { type: 'string' }, bind: { value: 'yes' } } } };
    graph.nodes.child = { type: 'pipeline', pipeline: { file: 'child.yaml' } };
    expect(sources(graph, new Map([['child.yaml', child]])).some(source => source.value === 'nodes.child.outputs.answer')).toBe(true);
  });
  it('allows a direct output binding without adding an explicit needs dependency', () => {
    expect(connectionError(researchPipeline.spec, { source: 'research', sourcePort: 'summary', target: 'draft', targetPort: 'summary' })).toBeUndefined();
  });
  it('rejects self links, missing ports, reverse dependencies and incompatible types', () => {
    expect(connectionError(researchPipeline.spec, { source: 'draft', sourcePort: 'brief', target: 'draft', targetPort: 'summary' })).toContain('itself');
    expect(connectionError(researchPipeline.spec, { source: 'research', sourcePort: 'missing', target: 'draft', targetPort: 'summary' })).toContain('named');
    expect(connectionError(researchPipeline.spec, { source: 'review', sourcePort: 'feedback', target: 'draft', targetPort: 'summary' })).toContain('cycle');
    expect(connectionError(researchPipeline.spec, { source: 'discover', sourcePort: 'result', target: 'draft', targetPort: 'summary' })).toContain('data types');
  });
  it('checks all input dependencies while allowing a binding replacement that removes a dependency', () => {
    const graph = structuredClone(researchPipeline.spec);
    graph.nodes.research.inputs!.sources.schema = { type: 'string' };
    expect(connectionError(graph, { source: 'review', sourcePort: 'feedback', target: 'research', targetPort: 'sources' })).toContain('cycle');
    graph.nodes.research.inputs!.sources.bind = { value: 'fresh input' };
    delete graph.nodes.draft.inputs!.summary;
    expect(connectionError(graph, { source: 'review', sourcePort: 'feedback', target: 'research', targetPort: 'sources' })).toBeUndefined();
  });
  it('keeps JSON and artifact wiring separate and checks file collections and MIME types', () => {
    const file = nodePorts(researchPipeline.spec.nodes.publish).document;
    expect(compatible(file, { schema: { type: 'object' } })).toBe(false);
    expect(compatible(file, { artifact: { mediaTypes: ['text/markdown'] } })).toBe(true);
    expect(compatible(file, { artifact: { mediaTypes: ['text/plain'] } })).toBe(false);
    expect(compatible(file, { artifact: { mediaTypes: ['text/markdown'], collection: true } })).toBe(false);
    expect(compatible({ schema: { type: 'integer' } }, { schema: { type: 'number' } })).toBe(true);
    expect(compatible({ schema: { type: ['string', 'null'] } }, { schema: { type: 'string' } })).toBe(false);
  });
});
