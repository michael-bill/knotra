import { useEffect, useRef } from 'react';
import { Compartment } from '@codemirror/state';
import { useTheme } from '../lib/theme';
import { EditorView } from '@codemirror/view';
import { basicSetup } from 'codemirror';
import { yaml } from '@codemirror/lang-yaml';
import { createEditorTheme } from './editorTheme';
export default function SourceEditor({ source, onChange, readOnly = false, label = 'Pipeline YAML' }: { source: string; onChange?: (source: string) => void; readOnly?: boolean; label?: string }) {
  const theme = useTheme();
  const themeCompartment = useRef(new Compartment());
  const container = useRef<HTMLDivElement>(null); const editor = useRef<EditorView | null>(null); const onChangeRef = useRef(onChange); onChangeRef.current = onChange;
  useEffect(() => {
    const view = new EditorView({ doc: source, parent: container.current!, extensions: [basicSetup, yaml(), EditorView.editable.of(!readOnly), EditorView.contentAttributes.of({ 'aria-label': label }), EditorView.updateListener.of(update => { if (update.docChanged) onChangeRef.current?.(update.state.doc.toString()); }), themeCompartment.current.of(createEditorTheme(theme))] });
    editor.current = view; return () => { view.destroy(); editor.current = null; };
  }, [readOnly, label]);
  useEffect(() => { editor.current?.dispatch({ effects: themeCompartment.current.reconfigure(createEditorTheme(theme)) }); }, [theme]);
  useEffect(() => { const view = editor.current; if (view && view.state.doc.toString() !== source) view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: source } }); }, [source]);
  return <div className="source-editor" ref={container} />;
}
