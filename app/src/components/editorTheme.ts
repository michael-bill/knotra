import { HighlightStyle, syntaxHighlighting, syntaxTree } from '@codemirror/language';
import { Decoration, EditorView, ViewPlugin, type DecorationSet, type ViewUpdate } from '@codemirror/view';
import type { Theme } from '../lib/theme';
import { tags } from '@lezer/highlight';

// Lezer YAML labels plain scalar values as content. Classify only those parsed
// values, so digits inside strings, comments, keys and prompt blocks stay intact.
function scalarDecorations(view: EditorView): DecorationSet {
  const marks: { from: number; to: number; value: Decoration }[] = [];
  for (const { from, to } of view.visibleRanges) syntaxTree(view.state).iterate({ from, to, enter(node) {
    if (node.name !== 'Literal' || node.node.parent?.name === 'Key') return;
    const value = view.state.sliceDoc(node.from, node.to);
    const kind = /^(true|false|null)$/.test(value) ? 'boolean' : /^-?(0|[1-9]\d*)(\.\d+)?([eE][+-]?\d+)?$/.test(value) ? 'number' : undefined;
    if (kind) marks.push({ from: node.from, to: node.to, value: Decoration.mark({ class: `knotra-scalar-${kind}` }) });
  } });
  return Decoration.set(marks.map(mark => mark.value.range(mark.from, mark.to)), true);
}
const scalars = ViewPlugin.fromClass(class {
  decorations: DecorationSet;
  constructor(view: EditorView) { this.decorations = scalarDecorations(view); }
  update(update: ViewUpdate) { if (update.docChanged || update.viewportChanged || syntaxTree(update.startState) !== syntaxTree(update.state)) this.decorations = scalarDecorations(update.view); }
}, { decorations: plugin => plugin.decorations });

export function createEditorTheme(theme: Theme) {
  const dark = theme === 'dark';
  const palette = dark ? {
    background: '#111413', foreground: '#e0e6e1', key: '#e7ece8',
    string: '#a7d9bb', number: '#c7c0e3', boolean: '#e2c38f',
    comment: '#929e95', punctuation: '#a5b2aa', gutter: '#7d8b82',
  } : {
    background: '#fbfcfa', foreground: '#24362b', key: '#203328',
    string: '#276747', number: '#70569b', boolean: '#885b18',
    comment: '#627169', punctuation: '#58695e', gutter: '#617168',
  };
  const syntax = HighlightStyle.define([
    { tag: [tags.propertyName, tags.definition(tags.propertyName)], color: palette.key, fontWeight: '500' },
    { tag: [tags.string, tags.special(tags.string), tags.content], color: palette.string },
    { tag: tags.number, color: palette.number },
    { tag: [tags.bool, tags.null, tags.keyword], color: palette.boolean },
    { tag: [tags.comment, tags.lineComment], color: palette.comment },
    { tag: [tags.separator, tags.punctuation, tags.brace, tags.squareBracket], color: palette.punctuation },
    { tag: [tags.meta, tags.typeName, tags.labelName], color: palette.number },
    { tag: tags.invalid, color: dark ? '#edaaa4' : '#ab443d', textDecoration: 'underline wavy' },
  ]);

  return [
    EditorView.theme({
      '&': { height: '100%', fontSize: '14px', backgroundColor: palette.background, color: palette.foreground },
      '.cm-scroller': { overflow: 'auto', fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', lineHeight: '1.85' },
      '.cm-content': { padding: '20px 0', caretColor: dark ? '#b4e8cb' : '#276747' },
      '.cm-line': { paddingLeft: '16px', paddingRight: '24px' },
      '.cm-gutters': { backgroundColor: dark ? '#0f1210' : '#f0f4ef', color: palette.gutter, borderRight: dark ? '1px solid #2c352f' : '1px solid #d5ddd4', padding: '0 7px 0 4px' },
      '.cm-activeLine': { backgroundColor: dark ? '#ffffff05' : '#1d4b2807' },
      '.cm-activeLineGutter': { backgroundColor: dark ? '#ffffff08' : '#1d4b280b', color: dark ? '#c5dacd' : '#2c5238' },
      '&.cm-focused .cm-selectionBackground, .cm-selectionBackground': { backgroundColor: dark ? '#42634f80' : '#bad9c7' },
      '.cm-cursor, .cm-dropCursor': { borderLeftColor: dark ? '#b4e8cb' : '#276747' },
      '.cm-matchingBracket': { backgroundColor: '#a1dec426', color: dark ? '#eff8f1' : '#214c32', outline: '1px solid #a1dec450', borderRadius: '2px' },
      '.cm-selectionMatch': { backgroundColor: '#a1dec416', outline: '1px solid #a1dec42c' },
      '.cm-foldPlaceholder': { backgroundColor: dark ? '#25382c' : '#e6eee5', border: dark ? '1px solid #526857' : '1px solid #bacbbd', color: dark ? '#bbd2c3' : '#33533d', borderRadius: '4px' },
      '.cm-panels': { backgroundColor: dark ? '#191e1a' : '#eff3ed', color: palette.foreground, borderColor: dark ? '#3c493f' : '#c7d4c9' },
      '.cm-searchMatch': { backgroundColor: '#bba67735', outline: '1px solid #d1bd8870' },
      '.cm-searchMatch-selected': { backgroundColor: '#bba67760' },
      '.cm-tooltip': { backgroundColor: dark ? '#202722' : '#ffffff', color: palette.foreground, border: dark ? '1px solid #4a5b50' : '1px solid #c7d4c9', borderRadius: '6px' },
      '.knotra-scalar-number': { color: palette.number },
      '.knotra-scalar-boolean': { color: palette.boolean },
      '&.cm-focused': { outline: 'none' },
    }, { dark }),
    syntaxHighlighting(syntax),
    scalars,
  ];
}
