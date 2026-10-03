import { base64, textBytes } from '../bytes';
import type { Workspace } from '../types';
import { allowedFiles } from '../package';
import type { EnginePackage } from './types';

// Drafts keep the editable source separately; the wire format carries every file.
export function enginePackage(
  workspace: Pick<Workspace, 'entrypoint' | 'source' | 'files'>,
): EnginePackage {
  return {
    entrypoint: workspace.entrypoint,
    source: workspace.source,
    files: [
      { path: workspace.entrypoint, content: base64(textBytes(workspace.source)) },
      ...allowedFiles(workspace).files,
    ],
  };
}
