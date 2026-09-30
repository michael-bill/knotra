export class EngineError extends Error {
  constructor(public code: string, message: string, public status?: number, public diagnostics: unknown[] = []) { super(message); }
}
export function engineError(error: unknown): EngineError { if (error instanceof EngineError) return error; const value = error as Partial<EngineError>; return new EngineError(value?.code ?? 'transport', value?.message ?? String(error), value?.status, value?.diagnostics); }
