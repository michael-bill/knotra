import type { EngineDiagnostic } from '../lib/engine/types';

export function Diagnostics({ diagnostics }: { diagnostics: EngineDiagnostic[] }) {
  return (
    <div className="engine-diagnostics">
      {diagnostics.map((diagnostic, index) => (
        <div className={`diagnostic diagnostic-${diagnostic.severity}`} key={index}>
          <div>
            <strong>{diagnostic.code}</strong>
            <p>{diagnostic.message}</p>
            <code>
              {diagnostic.file
                ? `${diagnostic.file}:${diagnostic.line}:${diagnostic.column} · `
                : ''}
              {diagnostic.path} · {diagnostic.phase}
            </code>
          </div>
        </div>
      ))}
    </div>
  );
}
