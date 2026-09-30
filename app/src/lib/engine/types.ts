import type { DataSchema, Json, PackageFile } from '../types';
export const PROTOCOL = 'knotra.desktop/1';
export type EngineStatus = 'pending' | 'ready' | 'running' | 'retry_wait' | 'waiting_human' | 'waiting_resolution' | 'succeeded' | 'skipped' | 'failed' | 'cancelled';
export interface EngineInfo { protocol: typeof PROTOCOL; engineId: string; principalId: string; version: string; capabilities: string[] }
export interface EnginePackage { entrypoint: string; source: string; files: PackageFile[] }
export interface EngineDiagnostic { severity: 'error' | 'warning'; code: string; phase: string; message: string; path: string; file?: string; line?: number; column?: number }
export interface EngineArtifact { id: string; name: string; mediaType: string; size: number; sha256: string; origin: { runId?: string; instanceId?: string; attemptId?: string; operationId?: string } }
export interface EngineEvent { id: string; runId: string; at: string; type: string; message: string; instanceId?: string; attemptId?: string; operationId?: string; data?: Json }
export interface EngineInstance { id: string; nodeId: string; scope: string; status: EngineStatus; attemptId?: string; error?: EngineDiagnostic }
export interface EngineRun { id: string; definitionId: string; title: string; status: EngineStatus; createdAt: string; updatedAt: string; profile: string; package: EnginePackage; inputs: Record<string, Json>; inputArtifacts: Record<string, string | string[]>; outputs: Record<string, Json>; artifacts: EngineArtifact[]; instances: EngineInstance[]; diagnostics: EngineDiagnostic[]; availableActions: ('cancel' | 'resume' | 'resolve')[] }
export interface EngineRequest { id: string; runId: string; instanceId: string; attemptId: string; status: 'open' | 'answered' | 'cancelled' | 'expired'; prompt: string; createdAt: string; deadline: string; inputs: { values: Record<string, Json>; artifacts: Record<string, EngineArtifact | EngineArtifact[]> }; responseSchema: DataSchema }
export interface Definition { id: string; name: string; title: string; packageDigest: string; createdAt: string; package: EnginePackage }
export interface Profile { id: string; title: string; revision: string }
export interface Resource { id: string; kind: 'model' | 'mcp' | 'sandbox' | 'secret'; title: string; capabilities: string[]; status: 'available' | 'unavailable' }
export interface Page<T> { items: T[]; nextCursor: string | null }
export type EngineCall =
 | { op: 'info' | 'definitions' | 'profiles' | 'resources' }
 | { op: 'runs' | 'requests' | 'artifacts'; cursor: string | null }
 | { op: 'run'; runId: string } | { op: 'definition'; definitionId: string } | { op: 'artifact'; artifactId: string }
 | { op: 'validate'; package: EnginePackage; profile: string; inputs: Record<string, Json>; artifacts: Record<string, string | string[]> }
 | { op: 'publish'; package: EnginePackage; operationId: string }
 | { op: 'start'; definitionId: string; profile: string; inputs: Record<string, Json>; artifacts: Record<string, string | string[]>; operationId: string }
 | { op: 'cancel' | 'resume'; runId: string; operationId: string }
 | { op: 'respond'; requestId: string; outputs: Record<string, Json>; operationId: string }
 | { op: 'resolve'; runId: string; instanceId: string; resolution: { outcome: 'succeeded' | 'not_started' | 'failed'; evidence: string; outputs?: Record<string, Json> }; operationId: string }
 | { op: 'upload'; file: PackageFile; mediaType: string; operationId: string };
export interface PendingOperation { id: string; request: EngineCall }
export interface EngineCache { cache: Record<string, unknown>; pending: PendingOperation[] }
export type StreamMessage = { type: 'event'; event: EngineEvent } | { type: 'connection'; status: string; message?: string };
export const terminal = (status: EngineStatus) => ['succeeded', 'skipped', 'failed', 'cancelled'].includes(status);
