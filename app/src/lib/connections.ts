import { dependencies, nodePorts } from './graph';
import { record, type Graph, type Port } from './types';

export interface PortConnection {
  source: string;
  sourcePort: string;
  target: string;
  targetPort: string;
}

export interface SourceOption {
  value: string;
  label: string;
  port: Port;
  node?: string;
  name: string;
}

export function sources(graph: Graph, imports?: Map<string, Graph>): SourceOption[] {
  return [
    ...Object.entries(graph.inputs ?? {}).map(([name, port]) => ({
      value: `inputs.${name}`,
      label: `Workflow input · ${name}`,
      port,
      name,
    })),
    ...Object.entries(graph.nodes).flatMap(([node, definition]) =>
      Object.entries(nodePorts(definition, imports)).map(([name, port]) => ({
        value: `nodes.${node}.outputs.${name}`,
        label: `${node} → ${name}`,
        port,
        node,
        name,
      })),
    ),
  ];
}

export function compatible(source: Port, target: Port): boolean {
  if (!!source.artifact !== !!target.artifact) return false;
  if (source.artifact && target.artifact)
    return (
      !!source.artifact.collection === !!target.artifact.collection &&
      source.artifact.mediaTypes.every((type) => target.artifact!.mediaTypes.includes(type))
    );
  const a = record(source.schema).type;
  const b = record(target.schema).type;
  if (!a || !b) return true; // Full schema compatibility is checked by the engine.
  const from = Array.isArray(a) ? a : [a];
  const to = Array.isArray(b) ? b : [b];
  return from.some((type) =>
    to.some(
      (targetType) =>
        type === targetType ||
        (type === 'integer' && targetType === 'number') ||
        (type === 'number' && targetType === 'integer'),
    ),
  );
}

export function connectionError(
  graph: Graph,
  connection: PortConnection,
  imports?: Map<string, Graph>,
): string | undefined {
  const source = graph.nodes[connection.source];
  const target = graph.nodes[connection.target];
  if (!source || !target) return 'Choose blocks in the same graph.';
  if (connection.source === connection.target) return 'A block cannot connect to itself.';
  const output = nodePorts(source, imports)[connection.sourcePort];
  const input = target.inputs?.[connection.targetPort];
  if (!output || !input) return 'Connect a named output to a named input.';
  if (!compatible(output, input))
    return 'These ports have different data types. Choose a compatible input or use a transformation.';
  const updated = {
    ...target,
    inputs: {
      ...target.inputs,
      [connection.targetPort]: {
        ...input,
        bind: { from: `nodes.${connection.source}.outputs.${connection.sourcePort}` },
      },
    },
  };
  const nodes = { ...graph.nodes, [connection.target]: updated };
  const visited = new Set<string>();
  function reaches(id: string): boolean {
    if (id === connection.target) return true;
    if (visited.has(id)) return false;
    visited.add(id);
    return dependencies(nodes[id]).some((dep) => nodes[dep] && reaches(dep));
  }
  if (reaches(connection.source))
    return 'This connection would create a cycle. Use a Loop block for repeated work.';
}
