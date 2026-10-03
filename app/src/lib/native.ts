import { isTauri, invoke } from '@tauri-apps/api/core';
import { base64, decodeText, textBytes } from './bytes';
import type { PackageFile } from './types';

export const desktop = isTauri();

export interface OpenedPackage {
  entrypoint: string;
  source: string;
  files: PackageFile[];
}

export async function openPackage(): Promise<OpenedPackage | null> {
  return invoke<OpenedPackage | null>('open_package');
}

export async function exportPackage(
  entrypoint: string,
  source: string,
  files: PackageFile[],
  name: string,
): Promise<string | null> {
  return invoke<string | null>('export_package', { entrypoint, source, files, name });
}

export async function exportFile(
  name: string,
  content: string,
  mediaType = 'text/plain',
): Promise<void> {
  if (desktop) {
    await invoke('export_file', { name, content: base64(textBytes(content)) });
    return;
  }
  download(name, content, mediaType);
}

export function download(name: string, content: string, mediaType: string): void {
  const url = URL.createObjectURL(new Blob([content], { type: mediaType }));
  const link = document.createElement('a');
  link.href = url;
  link.download = name;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

export async function browserPackage(files: File[], entrypoint: string): Promise<OpenedPackage> {
  const bytes = await Promise.all(
    files.map(async (file) => ({
      path: (file.webkitRelativePath || file.name).replace(
        /^[^/]+\//,
        file.webkitRelativePath ? '' : '$&',
      ),
      content: base64(new Uint8Array(await file.arrayBuffer())),
    })),
  );
  const root = files[0]?.webkitRelativePath.split('/')[0];
  const normalized = files[0]?.webkitRelativePath
    ? bytes.map((file, i) => ({
        ...file,
        path: files[i].webkitRelativePath.slice((root?.length ?? 0) + 1),
      }))
    : bytes;
  const entry = normalized.find((f) => f.path === entrypoint);
  if (!entry) throw new Error('Entrypoint file was not selected.');
  return {
    entrypoint,
    source: decodeText(entry.content),
    files: normalized.filter((f) => f.path !== entrypoint),
  };
}

export async function openProjectDocs(): Promise<void> {
  const url = 'https://github.com/michael-bill/knotra';
  if (desktop) {
    const { openUrl } = await import('@tauri-apps/plugin-opener');
    await openUrl(url);
  } else window.open(url, '_blank', 'noopener,noreferrer');
}
