export type NodeKind = 'llm' | 'agent' | 'code' | 'tool' | 'switch' | 'human' | 'foreach' | 'loop' | 'pipeline';
export type Json = null | boolean | number | string | Json[] | { [key: string]: Json };
export type DataSchema = boolean | Record<string, unknown>;
export interface Binding { from?: string; path?: string; expr?: string; value?: Json; coalesce?: Binding[] }
export interface Port {
  schema?: DataSchema; schemaRef?: string; artifact?: { mediaTypes: string[]; collection?: boolean };
  description?: string; required?: boolean; default?: Json; bind?: Binding; mount?: string;
  collect?: { path: string; mediaType: string };
}
export interface NodeDefinition {
  type: NodeKind; description?: string; inputs?: Record<string, Port>; outputs?: Record<string, Port>;
  needs?: string[]; when?: string; sandbox?: string; execution?: { retry?: { maxAttempts: number } };
  [key: string]: unknown;
}
export interface Graph { inputs?: Record<string, Port>; nodes: Record<string, NodeDefinition>; outputs: Record<string, Port> }
export interface Pipeline {
  apiVersion: 'knotra/v1'; kind: 'Pipeline'; metadata: { name: string; title?: string; description?: string; version?: string; labels?: Record<string, string> };
  spec: Graph & { files?: string[]; schemas?: Record<string, DataSchema>; models?: Record<string, Record<string, unknown>>;
    mcp?: Record<string, Record<string, unknown>>; sandboxes?: Record<string, Record<string, unknown>>;
    secrets?: Record<string, Record<string, unknown>>; defaults?: { execution?: { retry?: { maxAttempts: number } }; tools?: Record<string, unknown> } };
}
export interface PackageFile { path: string; content: string } // Bytes encoded as base64, including text files.
export interface Workspace { positions?: Record<string, { x: number; y: number }>; id: string; entrypoint: string; source: string; files: PackageFile[]; savedSource: string; updatedAt: string }
export interface Diagnostic { severity: 'error' | 'warning'; layer: 'parse' | 'structural' | 'package' | 'semantic' | 'admission'; code: string; message: string; path: string; line?: number }
export interface Validation { pipeline?: Pipeline; diagnostics: Diagnostic[] }
export type NodeStatus = 'pending' | 'ready' | 'retry_wait' | 'waiting_resolution' | 'running' | 'waiting_human' | 'succeeded' | 'skipped' | 'failed' | 'cancelled';
export type RunStatus = 'running' | 'waiting_human' | 'succeeded' | 'cancelled' | 'failed';
export interface RunEvent { id: string; at: string; node?: string; message: string }
export interface Artifact { id: string; name: string; mediaType: string; content: string; sha256: string; size: number }
export interface DemoRun {
  id: string; workspaceId: string; title: string; topic: string; createdAt: string; updatedAt: string;
  status: RunStatus; source: string; files: PackageFile[]; inputs: Record<string, Json>;
  nodes: Record<string, NodeStatus>; events: RunEvent[]; requestId?: string; response?: Record<string, Json>;
  outputs: Record<string, Json>; artifacts: Artifact[];
}
export type Section = 'pipelines' | 'runs' | 'inbox' | 'artifacts' | 'connections' | 'settings';
export function record(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : {};
}
