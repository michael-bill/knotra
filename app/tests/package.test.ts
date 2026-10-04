import { describe, expect, it } from 'vitest';
import { stringify } from 'yaml';
import { base64, decodeText, textBytes, unbase64 } from '../src/lib/bytes';
import { enginePackage } from '../src/lib/engine/package';
import { examples } from '../src/lib/examples';
import { parsePipeline } from '../src/lib/validation';

describe('desktop packages on the engine wire', () => {
  it('preserves a UTF-8 entrypoint BOM through decoding and publication', () => {
    const source = examples.find((example) => example.id === 'local')!.source;
    const bytes = Uint8Array.from([0xef, 0xbb, 0xbf, ...textBytes(source)]);
    const decoded = decodeText(base64(bytes));
    const pack = enginePackage({ entrypoint: 'pipeline.yaml', source: decoded, files: [] });
    expect(unbase64(pack.files[0].content)).toEqual(bytes);
    expect(parsePipeline(decoded)?.metadata.name).toBe('local-welcome');
  });
  it('publishes current entrypoint bytes and preserves declared binary files', () => {
    const pipeline = structuredClone(
      parsePipeline(examples.find((example) => example.id === 'local')!.source)!,
    );
    pipeline.spec.files = ['input.bin'];
    const bytes = Uint8Array.from([0, 255, 128, 13, 10]);
    const workspace = {
      entrypoint: 'main.yaml',
      source: stringify(pipeline),
      files: [
        { path: 'input.bin', content: base64(bytes) },
        { path: 'unused.txt', content: base64(textBytes('unused')) },
      ],
    };
    const pack = enginePackage(workspace);
    expect(pack.files.map((file) => file.path)).toEqual(['main.yaml', 'input.bin']);
    expect(decodeText(pack.files[0].content)).toBe(workspace.source);
    expect(unbase64(pack.files[1].content)).toEqual(bytes);
    expect(workspace.files).toHaveLength(2);
  });

  it('includes files declared by imported pipelines in the common package root', () => {
    const example = examples.find((example) => example.id === 'subpipeline')!;
    const child = parsePipeline(
      decodeText(example.files.find((file) => file.path === 'children/writer.yaml')!.content),
    )!;
    child.spec.files = ['nested.bin'];
    const pack = enginePackage({
      entrypoint: 'pipeline.yaml',
      source: example.source,
      files: [
        { path: 'children/writer.yaml', content: base64(textBytes(stringify(child))) },
        { path: 'nested.bin', content: 'AP8=' },
      ],
    });
    expect(pack.files.map((file) => file.path)).toEqual([
      'pipeline.yaml',
      'children/writer.yaml',
      'nested.bin',
    ]);
  });
});

it('treats a declared YAML file as data unless a node imports it', () => {
  const pipeline = structuredClone(
    parsePipeline(examples.find((example) => example.id === 'local')!.source)!,
  );
  pipeline.spec.files = ['notes.yaml'];
  const data = structuredClone(pipeline);
  data.spec.files = ['private.txt'];
  const pack = enginePackage({
    entrypoint: 'pipeline.yaml',
    source: stringify(pipeline),
    files: [
      { path: 'notes.yaml', content: base64(textBytes(stringify(data))) },
      { path: 'private.txt', content: base64(textBytes('not declared by an imported pipeline')) },
    ],
  });
  expect(pack.files.map((file) => file.path)).toEqual(['pipeline.yaml', 'notes.yaml']);
});
