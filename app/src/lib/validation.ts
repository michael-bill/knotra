import { Validator, type Schema } from '@cfworker/json-schema';
import { LineCounter, isAlias, isMap, isScalar, isSeq, parseAllDocuments, visit } from 'yaml';
import contract from '../../../schemas/knotra-v1.schema.json';
import { decodeText, textBytes, unbase64 } from './bytes';
import { graphOrder, nodePorts, nodeReferences, references } from './graph';
import {
  record,
  type Diagnostic,
  type Graph,
  type PackageFile,
  type Pipeline,
  type Port,
  type Validation,
} from './types';

const structural = new Validator(structuredClone(contract) as Schema, '2020-12', false);

function dataValidator(schema: unknown): Validator {
  return new Validator(structuredClone(schema) as Schema | boolean, '2020-12', false);
}

const MAX_YAML = 8 * 1024 * 1024;

export function validPath(path: string): boolean {
  return (
    path.length > 0 &&
    path.length <= 1024 &&
    !/[\\\x00-\x1f\x7f]/.test(path) &&
    !/^[a-z]:/i.test(path) &&
    path.split('/').every((p) => p && p !== '.' && p !== '..') &&
    path.normalize('NFC') === path
  );
}

function parse(source: string, diagnostics: Diagnostic[], filename: string): unknown {
  const add = (message: string, line?: number) =>
    diagnostics.push({
      severity: 'error',
      layer: 'parse',
      code: 'YAML_INVALID',
      message,
      path: filename,
      line,
    });
  if (textBytes(source).length > MAX_YAML) {
    add('Pipeline exceeds the 8 MiB YAML limit.');
    return;
  }
  const lineCounter = new LineCounter();
  const documents = parseAllDocuments(source, {
    version: '1.2',
    schema: 'core',
    uniqueKeys: true,
    lineCounter,
    keepSourceTokens: true,
  });
  if (documents.length !== 1 || !documents[0].contents) {
    add('Supply exactly one nonempty YAML document.');
    return;
  }
  const document = documents[0];
  for (const error of [...document.errors, ...document.warnings])
    add(error.message, error.linePos?.[0]?.line);
  if (
    /^%/m.test(source) &&
    source.split('\n').some((line) => line.startsWith('%') && line.trim() !== '%YAML 1.2')
  )
    add('Only the %YAML 1.2 directive is supported.');
  if (document.directives.yaml.version !== '1.2') add('The document must use YAML 1.2.');
  visit(document, (_key, node, path) => {
    if (path.length > 128) {
      add('YAML exceeds 64 levels of container nesting.');
      return visit.BREAK;
    }
    if (
      isAlias(node) ||
      ((isScalar(node) || isMap(node) || isSeq(node)) && (node.anchor || node.tag))
    )
      add(
        'Anchors, aliases and explicit tags are forbidden.',
        node.range ? lineCounter.linePos(node.range[0]).line : undefined,
      );
    if (isMap(node))
      for (const pair of node.items) {
        if (!isScalar(pair.key) || typeof pair.key.value !== 'string')
          add('Every map key must be a string.');
        if (isScalar(pair.key) && pair.key.value === '<<') add('YAML merge keys are forbidden.');
      }
    if (isScalar(node) && typeof node.value === 'number') {
      if (
        !/^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$/.test(node.source ?? '') ||
        !Number.isFinite(node.value)
      )
        add('Numeric literals must use finite JSON number syntax.');
      if (Number.isInteger(node.value) && !Number.isSafeInteger(node.value))
        add(
          'This integer requires the engine’s int64 parser; the desktop cannot represent it exactly.',
        );
      if (node.value === 0 && /[1-9]/.test((node.source ?? '').split(/[eE]/)[0]))
        add('Numeric underflow is forbidden.');
    }
  });
  if (diagnostics.some((d) => d.layer === 'parse' && d.path === filename)) return;
  return document.toJS({ maxAliasCount: 0 });
}

export function parsePipeline(source: string): Pipeline | undefined {
  try {
    const d: Diagnostic[] = [];
    const value = parse(source, d, 'pipeline.yaml');
    return !d.length && structural.validate(value).valid && record(value).kind === 'Pipeline'
      ? (value as Pipeline)
      : undefined;
  } catch {
    return;
  }
}

export function validatePipeline(
  source: string,
  files: PackageFile[] = [],
  entrypoint = 'pipeline.yaml',
): Validation {
  const diagnostics: Diagnostic[] = [];
  const error = (
    code: string,
    message: string,
    path: string,
    layer: Diagnostic['layer'] = 'semantic',
  ) => diagnostics.push({ severity: 'error', layer, code, message, path });
  let value: unknown;
  try {
    value = parse(source, diagnostics, entrypoint);
  } catch (e) {
    error('YAML_INVALID', String(e), entrypoint, 'parse');
  }
  if (diagnostics.length || !value) return { diagnostics };
  const structure = structural.validate(value);
  if (!structure.valid) {
    const unique = new Set<string>();
    // oneOf branches can generate many diagnostics. Prefer deepest concrete failures.
    const errors = structure.errors.filter((e) => !['oneOf', 'const', 'if'].includes(e.keyword));
    for (const issue of errors.slice(0, 24)) {
      const message = `${issue.instanceLocation || '/'} ${issue.error}`;
      if (!unique.has(message)) {
        unique.add(message);
        error('STRUCTURE_INVALID', message, issue.instanceLocation || '/', 'structural');
      }
    }
    if (!diagnostics.length)
      error(
        'STRUCTURE_INVALID',
        'Document does not match the Knotra v1 contract.',
        '/',
        'structural',
      );
    return { diagnostics };
  }
  if (record(value).kind !== 'Pipeline') {
    error(
      'PIPELINE_REQUIRED',
      'An EngineProfile is trusted engine configuration. Open a Pipeline in this workspace.',
      '/kind',
      'structural',
    );
    return { diagnostics };
  }
  const pipeline = value as Pipeline;
  if (!validPath(entrypoint))
    error('PATH_INVALID', 'Choose a normalized relative POSIX entrypoint.', entrypoint, 'package');
  const fileMap = new Map<string, PackageFile>();
  const folded = new Set<string>();
  let bytes = textBytes(source).length;
  for (const file of files) {
    const key = file.path.replace(/[A-Z]/g, (c) => c.toLowerCase());
    if (
      !validPath(file.path) ||
      folded.has(key) ||
      key === entrypoint.replace(/[A-Z]/g, (c) => c.toLowerCase())
    )
      error(
        'PATH_INVALID',
        `Invalid or conflicting package path: ${file.path}`,
        file.path,
        'package',
      );
    folded.add(key);
    fileMap.set(file.path, file);
    try {
      bytes += unbase64(file.content).length;
    } catch {
      error('PACKAGE_INVALID', 'File has invalid base64 contents.', file.path, 'package');
    }
  }
  if (files.length + 1 > 512 || bytes > 64 * 1024 * 1024)
    error('PACKAGE_LIMIT', 'Package exceeds 512 files or 64 MiB.', '/', 'package');
  const imported = new Map<string, Pipeline>();
  const active = new Set<string>([entrypoint]);
  const loaded = new Set<string>();
  function loadImports(p: Pipeline, depth = 0) {
    if (depth > 32) {
      error('IMPORT_DEPTH', 'Imports exceed 32 levels.', '/', 'package');
      return;
    }
    const allowed = new Set(p.spec.files ?? []);
    for (const path of allowed)
      if (!fileMap.has(path) && path !== entrypoint)
        error('FILE_MISSING', `Declared package file is missing: ${path}`, path, 'package');
    function walk(v: unknown, path: string) {
      if (Array.isArray(v)) {
        v.forEach((x, i) => walk(x, `${path}/${i}`));
        return;
      }
      for (const [key, x] of Object.entries(record(v))) {
        if (key === 'value') continue;
        if (key === 'file' && typeof x === 'string') {
          if (!allowed.has(x))
            error(
              'FILE_UNDECLARED',
              `Source file must appear in this Pipeline’s spec.files: ${x}`,
              path,
              'package',
            );
          if (!fileMap.has(x) && x !== entrypoint)
            error('FILE_MISSING', `Source file is missing: ${x}`, path, 'package');
        } else walk(x, `${path}/${key}`);
      }
    }
    walk(p.spec, '/spec');
    function graphImports(graph: Graph) {
      for (const node of Object.values(graph.nodes)) {
        if (node.type === 'pipeline') {
          const path = String(record(node.pipeline).file);
          if (active.has(path)) {
            error('IMPORT_CYCLE', `Recursive pipeline import: ${path}`, path, 'package');
            continue;
          }
          if (!fileMap.has(path) || loaded.has(path)) continue;
          try {
            const child = parse(decodeText(fileMap.get(path)!.content), diagnostics, path);
            if (!structural.validate(child).valid || record(child).kind !== 'Pipeline') {
              error('IMPORT_INVALID', 'Imported file must be a valid Pipeline.', path, 'package');
              continue;
            }
            imported.set(path, child as Pipeline);
            active.add(path);
            loadImports(child as Pipeline, depth + 1);
            active.delete(path);
            loaded.add(path);
          } catch {
            error('IMPORT_INVALID', 'Imported file must be UTF-8 YAML.', path, 'package');
          }
        }
        if (node.type === 'foreach' || node.type === 'loop')
          graphImports(record(record(node[node.type]).body) as unknown as Graph);
      }
    }
    graphImports(p.spec);
  }
  loadImports(pipeline);
  function check(p: Pipeline) {
    const schemas = new Map<string, unknown>();
    for (const [key, value] of Object.entries(p.spec.schemas ?? {})) {
      if (typeof record(value).file === 'string') {
        const path = String(record(value).file);
        if (!/\.(json|ya?ml)$/.test(path))
          error('SCHEMA_INVALID', 'Schema files must be JSON or YAML.', path);
        const file = fileMap.get(path);
        if (file)
          try {
            schemas.set(key, parse(decodeText(file.content), diagnostics, path));
          } catch {
            error('SCHEMA_INVALID', 'Cannot read schema as UTF-8.', path);
          }
      } else schemas.set(key, value);
    }
    function schemaOf(port: Port, path: string) {
      if (port.schemaRef && !schemas.has(port.schemaRef))
        error('REFERENCE_UNRESOLVED', `Unknown schema: ${port.schemaRef}`, path);
      return port.schemaRef ? schemas.get(port.schemaRef) : port.schema;
    }
    function checkPort(port: Port, path: string) {
      const schema = schemaOf(port, path);
      if (schema !== undefined)
        try {
          const validate = dataValidator(schema);
          if ('default' in port && !validate.validate(port.default).valid)
            error('DEFAULT_INVALID', 'Input default does not match its data schema.', path);
        } catch (e) {
          error('SCHEMA_INVALID', `Data schema cannot be compiled: ${String(e)}`, path);
        }
    }
    function checkGraph(graph: Graph, path: string, depth = 0) {
      if (depth > 32) {
        error('GRAPH_DEPTH', 'Graphs exceed 32 levels.', path);
        return;
      }
      for (const id of graphOrder(graph).cycle)
        error('GRAPH_CYCLE', `Dependency cycle includes ${id}.`, `${path}/nodes/${id}`);
      const inputNames = new Set(Object.keys(graph.inputs ?? {}));
      function refs(
        refs: string[],
        args: Set<string>,
        refPath: string,
        scope: 'graph' | 'node' | 'args' = 'node',
      ) {
        for (const ref of refs) {
          const parts = ref.split('.');
          if (parts[0] === 'nodes') {
            const node = graph.nodes[parts[1]];
            if (scope === 'args')
              error('SCOPE_INVALID', `Only args are visible here: ${ref}`, refPath);
            else if (
              !node ||
              !(parts[3] in nodePorts(node, new Map([...imported].map(([k, v]) => [k, v.spec]))))
            )
              error('REFERENCE_UNRESOLVED', `Unknown node or output: ${ref}`, refPath);
          } else if (parts[0] === 'inputs' && (scope === 'args' || !inputNames.has(parts[1])))
            error('REFERENCE_UNRESOLVED', `Input is unavailable in this scope: ${ref}`, refPath);
          else if (parts[0] === 'args' && (scope === 'graph' || !args.has(parts[1])))
            error('SCOPE_INVALID', `Argument is unavailable in this scope: ${ref}`, refPath);
          else if (!['nodes', 'inputs', 'args'].includes(parts[0]))
            error('SCOPE_INVALID', `Reference is unavailable in this scope: ${ref}`, refPath);
        }
      }
      Object.entries(graph.inputs ?? {}).forEach(([id, port]) =>
        checkPort(port, `${path}/inputs/${id}`),
      );
      for (const [id, node] of Object.entries(graph.nodes)) {
        const at = `${path}/nodes/${id}`;
        const args = new Set(Object.keys(node.inputs ?? {}));
        for (const need of node.needs ?? [])
          if (!graph.nodes[need]) error('REFERENCE_UNRESOLVED', `Unknown dependency: ${need}`, at);
        refs(nodeReferences(node), args, at);
        for (const [key, port] of Object.entries(node.inputs ?? {})) {
          checkPort(port, `${at}/inputs/${key}`);
          refs(references(port.bind), new Set(), `${at}/inputs/${key}`, 'graph');
          if (port.artifact && ['agent', 'code'].includes(node.type) !== Boolean(port.mount))
            error(
              'ARTIFACT_MOUNT',
              'Artifact inputs in agent/code require mount; other types forbid it.',
              `${at}/inputs/${key}`,
            );
          if (!port.artifact && port.mount)
            error('ARTIFACT_MOUNT', 'JSON inputs cannot have a mount.', `${at}/inputs/${key}`);
          if (
            port.artifact &&
            port.bind &&
            (port.bind.expr || 'value' in port.bind || port.bind.path !== undefined)
          )
            error(
              'ARTIFACT_BINDING',
              'Artifacts require from/coalesce bindings without JSON Pointer.',
              `${at}/inputs/${key}`,
            );
        }
        for (const [key, port] of Object.entries(node.outputs ?? {})) {
          checkPort(port, `${at}/outputs/${key}`);
          if (port.collect && !port.artifact?.mediaTypes.includes(port.collect.mediaType))
            error(
              'MEDIA_TYPE_INVALID',
              'Collected media type is outside the declared contract.',
              at,
            );
        }
        if (
          ['llm', 'agent'].includes(node.type) &&
          !p.spec.models?.[String(record(node[node.type]).model)]
        )
          error('REFERENCE_UNRESOLVED', 'Node model alias is not declared in spec.models.', at);
        if (node.sandbox && !p.spec.sandboxes?.[node.sandbox])
          error('REFERENCE_UNRESOLVED', 'Sandbox alias is not declared.', at);
        if (node.type === 'tool') {
          if (!p.spec.mcp?.[String(record(node.tool).server)])
            error('REFERENCE_UNRESOLVED', 'MCP server alias is not declared.', at);
          refs(references({ arguments: record(node.tool).arguments }), args, at, 'args');
        }
        const retries =
          node.execution?.retry?.maxAttempts ?? p.spec.defaults?.execution?.retry?.maxAttempts ?? 1;
        if (['human', 'switch', 'foreach', 'loop', 'pipeline'].includes(node.type) && retries !== 1)
          error('RETRY_INVALID', `${node.type} requires exactly one attempt.`, at);
        if (node.type === 'switch') {
          const cases = record(node.switch).cases as { name: string; when: string }[];
          const names = [...cases.map((c) => c.name), String(record(node.switch).default)];
          if (new Set(names).size !== names.length)
            error('ROUTE_DUPLICATE', 'Switch routes and default must be distinct.', at);
          cases.forEach((c) => refs(references({ when: c.when }), args, at, 'args'));
        }
        if (node.type === 'foreach' || node.type === 'loop') {
          const config = record(node[node.type]);
          const body = config.body as Graph;
          const supplied = record(config.with);
          for (const key of Object.keys(supplied))
            if (!body.inputs?.[key])
              error('REFERENCE_UNRESOLVED', `Unknown body input: ${key}`, at);
          for (const [key, port] of Object.entries(body.inputs ?? {}))
            if (!(key in supplied) && port.required !== false && !('default' in port))
              error('INPUT_UNAVAILABLE', `Required body input has no binding: ${key}`, at);
          if (node.type === 'foreach') {
            const over = node.inputs?.[String(config.over)];
            const schema = over ? record(schemaOf(over, at)) : {};
            if (!over || (schema.type !== 'array' && !over.artifact?.collection))
              error(
                'FOREACH_TYPE',
                'foreach.over must reference an array or artifact collection input.',
                at,
              );
            for (const port of Object.values(body.outputs))
              if (port.required === false || port.artifact?.collection)
                error(
                  'FOREACH_EXPORT',
                  'Foreach body exports must be required and cannot be artifact collections.',
                  at,
                );
          } else if ('iterations' in body.outputs || 'termination' in body.outputs)
            error(
              'LOOP_EXPORT',
              'Loop body cannot export reserved iterations/termination ports.',
              at,
            );
          checkGraph(body, `${at}/${node.type}/body`, depth + 1);
        }
      }
      for (const [id, port] of Object.entries(graph.outputs)) {
        checkPort(port, `${path}/outputs/${id}`);
        refs(references(port.bind), new Set(), `${path}/outputs/${id}`, 'graph');
      }
    }
    checkGraph(p.spec, '/spec');
  }
  check(pipeline);
  for (const child of imported.values()) check(child);
  diagnostics.push({
    severity: 'warning',
    layer: 'semantic',
    code: 'ENGINE_CHECKS_REQUIRED',
    message:
      'Full CEL typechecking, schema compatibility, permissions and execution admission require the engine validator.',
    path: '/',
  });
  diagnostics.push({
    severity: 'warning',
    layer: 'admission',
    code: 'ENGINE_ADMISSION_REQUIRED',
    message:
      'Run admission checks on the connected engine to verify resources, permissions and inputs.',
    path: '/',
  });
  return { pipeline, diagnostics };
}

export function validateResponse(pipeline: Pipeline, nodeId: string, value: unknown): string[] {
  const node = pipeline.spec.nodes[nodeId];
  if (!node || node.type !== 'human') return ['Human request node does not exist.'];
  const ports = node.outputs ?? {};
  const object = record(value);
  const errors: string[] = [];
  if (!value || typeof value !== 'object' || Array.isArray(value))
    return ['Response must be a JSON object keyed by output port.'];
  for (const key of Object.keys(object)) if (!(key in ports)) errors.push(`Unknown output: ${key}`);
  for (const [key, port] of Object.entries(ports)) {
    if (!(key in object)) {
      if (port.required !== false) errors.push(`Missing required output: ${key}`);
      continue;
    }
    try {
      const schema = port.schemaRef ? pipeline.spec.schemas?.[port.schemaRef] : port.schema;
      if (schema === undefined || typeof record(schema).file === 'string') {
        errors.push(`Schema for ${key} requires engine resolution.`);
        continue;
      }
      const result = dataValidator(schema).validate(object[key]);
      if (!result.valid) errors.push(`${key}: ${result.errors.map((e) => e.error).join(', ')}`);
    } catch {
      errors.push(`Invalid schema for ${key}.`);
    }
  }
  return errors;
}
