import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import ts from 'typescript';
import { describe, expect, it } from 'vitest';
import { normalizeLocale, preferredLocale, translate, translateMessage } from '../src/lib/i18n';
import englishCatalog from '../src/locales/en.json';
import russianCatalog from '../src/locales/ru.json';
import { freshState, readBackup } from '../src/lib/storage';

const englishMessages: Record<string, string> = englishCatalog;
const russianMessages: Record<string, string> = russianCatalog;

describe('interface language contract', () => {
  it('keeps unknown content verbatim in both languages', () => {
    const content = "User / API message: {name}\n$& $` $' — Привет";
    expect(translate(content, 'en')).toBe(content);
    expect(translate(content, 'ru')).toBe(content);
    expect(translateMessage(content, 'en')).toBe(content);
    expect(translateMessage(content, 'ru')).toBe(content);
  });

  it('translates known frontend diagnostics while preserving the captured filename verbatim', () => {
    const path = "$& $` $' {name}\nПапка/report.csv";
    const diagnostic = `Declared package file is missing: ${path}`;
    expect(translateMessage(diagnostic, 'en')).toBe(diagnostic);
    expect(translateMessage(diagnostic, 'ru')).toBe(`Отсутствует объявленный файл пакета: ${path}`);
    expect(translateMessage('Error: Archive exceeds 64 MiB.', 'ru')).toBe(
      'Размер архива превышает 64 МиБ.',
    );
    expect(translateMessage('Error: Archive exceeds 64 MiB.', 'en')).toBe(
      'Error: Archive exceeds 64 MiB.',
    );
    expect(translateMessage('Error: User/API message {name}\n$&', 'ru')).toBe(
      'Error: User/API message {name}\n$&',
    );
  });

  it('preserves source identifiers and newlines while translating frontend dependency errors', () => {
    const sourceID = "$& $` $' {count}\nSourceID — Имя";
    const diagnostic = `Unknown dependency: ${sourceID}`;
    expect(translateMessage(diagnostic, 'en')).toBe(diagnostic);
    expect(translateMessage(diagnostic, 'ru')).toBe(`Неизвестная зависимость: ${sourceID}`);
  });

  it('inserts parameter values verbatim without treating dollars or braces as replacements', () => {
    const name = "$& $` $' {count}\nUser alias — Пользователь";
    expect(translate('resources.logicalResourcesDeclaredByName', 'en', { name })).toBe(
      `Logical resources declared by ${name}.`,
    );
    expect(translate('resources.logicalResourcesDeclaredByName', 'ru', { name })).toBe(
      `Логические ресурсы, объявленные в ${name}.`,
    );
    expect(translate('resources.profilesProfilesResourcesResources', 'en', { profiles: 0 })).toBe(
      '0 profiles · {resources} resources',
    );
  });

  it('preserves named placeholders between English and Russian', () => {
    const placeholders = (text: string) =>
      [...text.matchAll(/\{(\w+)\}/g)].map((match) => match[1]).sort();
    expect(Object.keys(russianMessages).sort()).toEqual(Object.keys(englishMessages).sort());
    const mismatches = Object.entries(englishMessages)
      .filter(
        ([key, english]) =>
          JSON.stringify(placeholders(english)) !==
          JSON.stringify(placeholders(russianMessages[key])),
      )
      .map(([key]) => key);
    expect(mismatches).toEqual([]);
  });

  it('chooses a supported interface language and falls back for unsupported saved values', () => {
    expect(preferredLocale('ru')).toBe('ru');
    expect(preferredLocale('RU-RU')).toBe('ru');
    expect(preferredLocale('en-US')).toBe('en');
    expect(preferredLocale('de-DE')).toBe('en');
    expect(normalizeLocale('ru')).toBe('ru');
    expect(normalizeLocale('en')).toBe('en');
    for (const value of [undefined, null, 'de', 'RU', 1, {}])
      expect(normalizeLocale(value)).toBe(preferredLocale());
  });

  it('restores legacy and unsupported-locale backups without changing authoring content', () => {
    const original = freshState();
    const legacy = { ...original } as Record<string, unknown>;
    delete legacy.locale;
    for (const backup of [legacy, { ...legacy, locale: 'de' }]) {
      const restored = readBackup(JSON.stringify(backup));
      expect(restored.locale).toBe(preferredLocale());
      expect(restored.workspaces).toEqual(original.workspaces);
      expect(restored.runs).toEqual(original.runs);
      expect(restored.activeId).toBe(original.activeId);
      expect(restored.engineUrl).toBe(original.engineUrl);
      expect(restored.theme).toBe(original.theme);
    }
  });

  it('covers literal interface keys in source in both locale files', () => {
    const root = resolve(dirname(fileURLToPath(import.meta.url)), '../src');
    const protocolTokens = new Set(['JSON', 'YAML', 'MCP', 'SHA-256', 'Knotra']);
    const missing = new Set<string>();
    function strings(expression: ts.Expression): string[] {
      if (ts.isStringLiteralLike(expression)) return [expression.text];
      if (ts.isParenthesizedExpression(expression)) return strings(expression.expression);
      if (ts.isConditionalExpression(expression))
        return [...strings(expression.whenTrue), ...strings(expression.whenFalse)];
      return [];
    }
    function walk(directory: string) {
      for (const entry of readdirSync(directory, { withFileTypes: true })) {
        const path = join(directory, entry.name);
        if (entry.isDirectory()) {
          walk(path);
          continue;
        }
        if (!/\.tsx?$/.test(entry.name)) continue;
        const source = ts.createSourceFile(
          path,
          readFileSync(path, 'utf8'),
          ts.ScriptTarget.Latest,
          true,
          entry.name.endsWith('.tsx') ? ts.ScriptKind.TSX : ts.ScriptKind.TS,
        );
        function visit(node: ts.Node) {
          if (
            ts.isCallExpression(node) &&
            ts.isIdentifier(node.expression) &&
            ['t', 'translate'].includes(node.expression.text) &&
            node.arguments[0]
          )
            for (const message of strings(node.arguments[0]))
              if (
                (!Object.hasOwn(englishMessages, message) ||
                  !Object.hasOwn(russianMessages, message)) &&
                !protocolTokens.has(message)
              ) {
                const { line } = source.getLineAndCharacterOfPosition(node.getStart(source));
                missing.add(`${relative(root, path)}:${line + 1}: ${message}`);
              }
          ts.forEachChild(node, visit);
        }
        visit(source);
      }
    }
    walk(root);
    expect([...missing].sort()).toEqual([]);
  });
});
