import { assertEngineModel } from '../src/lib/engine/validation';
import { freshState, readBackup } from '../src/lib/storage';
import { describe, expect, it } from 'vitest';
import { EventParser } from '../src/lib/engine/client';

describe('durable event framing', () => {
  it('handles arbitrary boundaries, CRLF, multiline data and heartbeat comments', () => {
    const parser = new EventParser();
    const frames = [];
    for (const char of ': heartbeat\r\nid: e1\r\ndata: {"message":"Привет",\r\ndata: "n":1}\r\n\r\n')
      frames.push(...parser.push(char));
    expect(frames).toEqual([{ id: 'e1', data: '{"message":"Привет",\n"n":1}' }]);
  });
  it('requires durable IDs and rejects oversized/injectable messages', () => {
    expect(() => new EventParser().push('data: {}\n\n')).toThrow('durable ID');
    expect(() => new EventParser().push('id: bad\0id\ndata: {}\n\n')).toThrow('Invalid event ID');
    expect(() => new EventParser().push('x'.repeat(256 * 1024 + 1))).toThrow('256 KiB');
    expect(() => new EventParser().push('я'.repeat(128 * 1024 + 1))).toThrow('256 KiB');
  });
});

it('checks engine models against the documented contract rather than trusting JSON', () => {
  expect(() =>
    assertEngineModel('Info', {
      protocol: 'knotra.desktop/1',
      engineId: 'fixture',
      principalId: 'test-user',
      version: '1',
      capabilities: [],
    }),
  ).not.toThrow();
  expect(() => assertEngineModel('Run', { id: 'partial', status: 'succeeded' })).toThrow(
    'invalid Run',
  );
  expect(() =>
    assertEngineModel('Info', {
      protocol: 'other',
      engineId: 'fixture',
      principalId: 'test-user',
      version: '1',
      capabilities: [],
    }),
  ).toThrow('invalid Info');
});

it('restores complete authoring bytes and rejects ambiguous or hostile backups', () => {
  const original = freshState();
  original.theme = 'light';
  expect(readBackup(JSON.stringify(original))).toEqual(original);
  original.workspaces.push(original.workspaces[0]);
  expect(() => readBackup(JSON.stringify(original))).toThrow('Invalid pipeline');
  original.workspaces.pop();
  original.workspaces[0].files.push({ path: '../escape', content: 'AA==' });
  expect(() => readBackup(JSON.stringify(original))).toThrow('package path');
});

it('uses the engine ASCII path folding rules when restoring supporting files', () => {
  const original = freshState();
  original.workspaces[0].files = [
    { path: 'Ä.txt', content: 'AA==' },
    { path: 'ä.txt', content: 'AA==' },
  ];
  expect(readBackup(JSON.stringify(original))).toEqual(original);
  original.workspaces[0].files.push({ path: 'DATA.txt', content: 'AA==' });
  original.workspaces[0].files.push({ path: 'data.txt', content: 'AA==' });
  expect(() => readBackup(JSON.stringify(original))).toThrow('package path');
});
