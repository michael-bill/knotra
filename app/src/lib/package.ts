import { decodeText } from './bytes';
import { record, type Graph, type PackageFile } from './types';
import { parsePipeline } from './validation';

type DraftPackage = { entrypoint: string; source: string; files: PackageFile[] };

function imports(graph: Graph): string[] {
  return Object.values(graph.nodes ?? {}).flatMap((node) => {
    if (node.type === 'pipeline') return [String(record(node.pipeline).file)];
    if (node.type === 'foreach' || node.type === 'loop') {
      return imports(record(record(node[node.type]).body) as unknown as Graph);
    }
    return [];
  });
}

// Every imported Pipeline uses the same package root and declares its own files.
export function allowedFiles<T extends DraftPackage>(opened: T): T {
  const files = new Map(opened.files.map((file) => [file.path, file]));
  const declared = new Set<string>();
  const visited = new Set<string>();

  function walk(source: string, path: string) {
    if (visited.has(path)) return;
    visited.add(path);
    if (visited.size > 512) throw new Error('Too many package files.');
    const pipeline = parsePipeline(source);
    if (!pipeline) return;
    for (const name of pipeline.spec.files ?? []) declared.add(name);

    for (const name of imports(pipeline.spec)) {
      const file = files.get(name);
      if (!file || !(pipeline.spec.files ?? []).includes(name)) continue;
      let child: string;
      try {
        child = decodeText(file.content);
      } catch {
        continue; // Package validation reports unreadable imported files.
      }
      walk(child, name);
    }
  }

  walk(opened.source, opened.entrypoint);
  return { ...opened, files: opened.files.filter((file) => declared.has(file.path)) };
}
