import { record, type Graph, type NodeDefinition, type Port } from './types';

export function references(value: unknown): string[] {
  if (!value || typeof value !== 'object') return [];
  if (Array.isArray(value)) return value.flatMap(references);
  const result: string[] = [];
  for (const [key, item] of Object.entries(value)) {
    if (key === 'value') continue;
    if (typeof item === 'string' && key === 'from') result.push(item);
    else if (typeof item === 'string' && ['expr', 'when', 'until'].includes(key)) {
      // Static references only. Full CEL parsing/typechecking belongs to the engine.
      const expression = item.replace(/"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'/g, ' ');
      result.push(...Array.from(expression.matchAll(/\b(?:nodes\.[a-z][a-z0-9_]*\.outputs\.[a-z][a-z0-9_]*|(?:inputs|args|state)\.[a-z][a-z0-9_]*|body\.outputs\.[a-z][a-z0-9_]*)/g), m => m[0]));
    } else if (!['body', 'state'].includes(key)) result.push(...references(item));
  }
  return [...new Set(result)];
}
export function nodeReferences(node: NodeDefinition): string[] { return references({ inputs: node.inputs, when: node.when }); }
export function dependencies(node: NodeDefinition): string[] {
  return [...new Set([...(node.needs ?? []), ...nodeReferences(node).filter(r => r.startsWith('nodes.')).map(r => r.split('.')[1])])];
}
export function nodePorts(node: NodeDefinition, files: Map<string, Graph> = new Map()): Record<string, Port> {
  if (node.type === 'switch') return { route: { schema: { type: 'string' } } };
  if (node.type === 'foreach' || node.type === 'loop') {
    const body = record(record(node[node.type]).body) as unknown as Graph;
    const outputs = { ...(body.outputs ?? {}) };
    if (node.type === 'loop') return { ...outputs, iterations: { schema: { type: 'integer' } }, termination: { schema: { type: 'string' } } };
    return Object.fromEntries(Object.entries(outputs).map(([key, port]) => [key, port.artifact ? { ...port, artifact: { ...port.artifact, collection: true } } : { ...port, schema: { type: 'array', items: port.schema ?? true } }]));
  }
  if (node.type === 'pipeline') return files.get(String(record(node.pipeline).file))?.outputs ?? {};
  return node.outputs ?? {};
}
export function graphOrder(graph: Graph): { order: string[]; cycle: string[] } {
  const order: string[] = []; const visiting = new Set<string>(); const visited = new Set<string>(); const cycle = new Set<string>();
  function visit(id: string) {
    if (visiting.has(id)) { cycle.add(id); return; }
    if (visited.has(id) || !graph.nodes[id]) return;
    visiting.add(id); for (const dep of dependencies(graph.nodes[id])) visit(dep);
    visiting.delete(id); visited.add(id); order.push(id);
  }
  Object.keys(graph.nodes).forEach(visit);
  return { order, cycle: [...cycle] };
}
export function portType(port: Port): string {
  if (port.artifact) return port.artifact.collection ? 'files' : 'file';
  if (port.schemaRef) return port.schemaRef;
  const schema = record(port.schema);
  return Array.isArray(schema.type) ? schema.type.join(' | ') : String(schema.type ?? (schema.enum ? 'enum' : 'JSON'));
}
