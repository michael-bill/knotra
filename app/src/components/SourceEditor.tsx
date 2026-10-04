import { useEffect, useRef } from 'react';
import { Compartment, EditorState } from '@codemirror/state';
import { useI18n } from '../lib/i18n';
import { useTheme } from '../lib/theme';
import { EditorView } from '@codemirror/view';
import { basicSetup } from 'codemirror';
import { yaml } from '@codemirror/lang-yaml';
import { createEditorTheme } from './editorTheme';

const editorPhrases = [
  ['Find', 'editor.find'],
  ['Replace', 'editor.replace'],
  ['next', 'editor.next'],
  ['previous', 'editor.previous'],
  ['all', 'editor.all'],
  ['match case', 'editor.matchCase'],
  ['regexp', 'editor.regexp'],
  ['by word', 'editor.byWord'],
  ['replace', 'editor.replace2'],
  ['replace all', 'editor.replaceAll'],
  ['close', 'editor.close'],
  ['current match', 'editor.currentMatch'],
  ['on line', 'editor.onLine'],
  ['replaced match on line $', 'editor.replacedMatchOnLine'],
  ['replaced $ matches', 'editor.replacedMatches'],
  ['Go to line', 'editor.goToLine'],
  ['go', 'editor.go'],
  ['Diagnostics', 'editor.diagnostics'],
  ['No diagnostics', 'editor.noDiagnostics'],
  ['Completions', 'editor.completions'],
  ['Control character', 'editor.controlCharacter'],
  ['Folded lines', 'editor.foldedLines'],
  ['Unfolded lines', 'editor.unfoldedLines'],
  ['to', 'editor.to'],
  ['folded code', 'editor.foldedCode'],
  ['unfold', 'editor.unfold'],
  ['Fold line', 'editor.foldLine'],
  ['Unfold line', 'editor.unfoldLine'],
  ['Selection deleted', 'editor.selectionDeleted'],
] as const;

export default function SourceEditor({
  source,
  onChange,
  readOnly = false,
  label = 'editor.pipelineYaml',
}: {
  source: string;
  onChange?: (source: string) => void;
  readOnly?: boolean;
  label?: string;
}) {
  const { t, locale } = useI18n();
  const theme = useTheme();
  const themeCompartment = useRef(new Compartment());
  const languageCompartment = useRef(new Compartment());
  const container = useRef<HTMLDivElement>(null);
  const editor = useRef<EditorView | null>(null);
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  useEffect(() => {
    const view = new EditorView({
      doc: source,
      parent: container.current!,
      extensions: [
        basicSetup,
        yaml(),
        EditorView.editable.of(!readOnly),
        languageCompartment.current.of([
          EditorView.contentAttributes.of({ 'aria-label': t(label) }),
          EditorState.phrases.of(
            Object.fromEntries(editorPhrases.map(([phrase, key]) => [phrase, t(key)])),
          ),
        ]),
        EditorView.updateListener.of((update) => {
          if (update.docChanged) onChangeRef.current?.(update.state.doc.toString());
        }),
        themeCompartment.current.of(createEditorTheme(theme)),
      ],
    });
    editor.current = view;
    return () => {
      view.destroy();
      editor.current = null;
    };
  }, [readOnly]);
  useEffect(() => {
    editor.current?.dispatch({
      effects: languageCompartment.current.reconfigure([
        EditorView.contentAttributes.of({ 'aria-label': t(label) }),
        EditorState.phrases.of(
          Object.fromEntries(editorPhrases.map(([phrase, key]) => [phrase, t(key)])),
        ),
      ]),
    });
  }, [label, locale, t]);
  useEffect(() => {
    editor.current?.dispatch({
      effects: themeCompartment.current.reconfigure(createEditorTheme(theme)),
    });
  }, [theme]);
  useEffect(() => {
    const view = editor.current;
    if (view && view.state.doc.toString() !== source)
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: source } });
  }, [source]);
  return <div className="source-editor" ref={container} />;
}
