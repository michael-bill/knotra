import { editorDataTypeLabel } from '../lib/editorLabels';
import { useI18n } from '../lib/i18n';
import { useEffect, useRef, useState } from 'react';
import { Plus, Trash2, Settings2 } from 'lucide-react';
import { sources } from '../lib/connections';
import { record, type Graph, type Pipeline, type Port } from '../lib/types';
import { BindingEditor, seedValue, JsonField, portDataType } from './PortEditor';

export default function WorkflowSetup({
  pipeline,
  graph,
  imports,
  scope,
  onGraphEdit,
  onMetadata,
  onResources,
  showResources,
}: {
  showResources?: boolean;
  pipeline: Pipeline;
  graph: Graph;
  imports: Map<string, Graph>;
  scope: string[];
  onGraphEdit: (key: 'inputs' | 'outputs', value: Record<string, Port>) => void;
  onMetadata: (title: string) => void;
  onResources: (
    kind: 'models' | 'mcp' | 'sandboxes',
    value: Record<string, Record<string, unknown>>,
  ) => void;
}) {
  const { t, message } = useI18n();
  const resourcePanel = useRef<HTMLElement>(null);
  useEffect(() => {
    if (showResources) resourcePanel.current?.scrollIntoView({ block: 'start' });
  }, [showResources]);
  const [input, setInput] = useState('');
  const [type, setType] = useState('string');
  const [output, setOutput] = useState('');
  const [source, setSource] = useState('');
  const [error, setError] = useState('');
  const options = sources(graph, imports);
  const validName = (name: string, existing: Record<string, unknown>) =>
    /^[a-z][a-z0-9_]{0,63}$/.test(name) && !(name in existing);
  return (
    <aside className="inspector setup-inspector">
      <header className="inspector-header">
        <span className="eyebrow">
          {t(scope.length ? 'editor.bodyGraph' : 'editor.workflowSettings')}
        </span>
        <Settings2 size={16} />
      </header>
      <div className="inspector-content">
        {!scope.length ? (
          <label className="field">
            {t('editor.workflowTitle')}
            <input
              aria-label={t('editor.workflowTitle')}
              defaultValue={pipeline.metadata.title ?? pipeline.metadata.name}
              key={pipeline.metadata.title}
              onBlur={(event) => {
                if (event.target.value.trim()) onMetadata(event.target.value);
              }}
            />
          </label>
        ) : (
          <p className="setup-hint">{t('editor.inputsAndExportsBelongToThisBodyGraph')}</p>
        )}
        <section className="port-editor">
          <h3>{t('editor.workflowInputs')}</h3>
          <p className="setup-hint">{t('editor.startingValuesForARunBlocksCanChoose')}</p>
          {Object.entries(graph.inputs ?? {}).map(([name, port]) => (
            <div className="port-editor-card" key={name}>
              <div className="inline spread">
                <strong>{name}</strong>
                <button
                  className="icon-button"
                  aria-label={t('editor.removeWorkflowInputName', { name })}
                  onClick={() => {
                    const next = { ...graph.inputs };
                    delete next[name];
                    onGraphEdit('inputs', next);
                  }}
                >
                  <Trash2 size={13} />
                </button>
              </div>
              {port.artifact ? (
                <small>
                  {t('editor.file')} · {port.artifact.mediaTypes.join(', ')}
                </small>
              ) : (
                <>
                  <label className="field">
                    {t('editor.inputType')}
                    <select
                      value={port.schemaRef ? '$custom' : portDataType(port)}
                      onChange={(event) => {
                        const next: Port = {
                          ...port,
                          schema:
                            event.target.value === 'JSON' ? true : { type: event.target.value },
                        };
                        delete next.schemaRef;
                        if ('default' in next) next.default = seedValue(next);
                        onGraphEdit('inputs', { ...graph.inputs, [name]: next });
                      }}
                    >
                      {port.schemaRef || portDataType(port) === '$custom' ? (
                        <option value="$custom">
                          {t('editor.customSchema')}
                          {port.schemaRef ? ` · ${port.schemaRef}` : ''}
                        </option>
                      ) : null}
                      {['string', 'object', 'array', 'integer', 'number', 'boolean', 'JSON'].map(
                        (type) => (
                          <option key={type} value={type}>
                            {editorDataTypeLabel(type, t)}
                          </option>
                        ),
                      )}
                    </select>
                  </label>
                  {'default' in port ? (
                    <JsonField
                      label={t('editor.defaultForName', { name })}
                      value={port.default}
                      onSave={(value) =>
                        onGraphEdit('inputs', {
                          ...graph.inputs,
                          [name]: { ...port, default: value },
                        })
                      }
                      hint={t('editor.jsonValueStringsUseQuotes')}
                    />
                  ) : null}
                </>
              )}
              <details className="field-details">
                <summary>{t('editor.schemaDetails')}</summary>
                <pre>
                  {JSON.stringify(
                    port.schema ?? port.artifact ?? { schemaRef: port.schemaRef },
                    null,
                    2,
                  )}
                </pre>
              </details>
            </div>
          ))}
          <div className="port-add-form">
            <label className="field">
              {t('editor.newWorkflowInput')}
              <input
                value={input}
                onChange={(event) => setInput(event.target.value)}
                placeholder="topic"
              />
            </label>
            <label className="field">
              {t('editor.dataType')}
              <select value={type} onChange={(event) => setType(event.target.value)}>
                {['string', 'object', 'array', 'integer', 'number', 'boolean'].map((type) => (
                  <option key={type} value={type}>
                    {editorDataTypeLabel(type, t)}
                  </option>
                ))}
              </select>
            </label>
            <button
              className="button small-button"
              onClick={() => {
                if (!validName(input, graph.inputs ?? {})) {
                  setError('Choose a unique lowercase input name.');
                  return;
                }
                const port: Port = {
                  schema: { type, ...(type === 'array' ? { items: true } : {}) },
                };
                onGraphEdit('inputs', {
                  ...graph.inputs,
                  [input]: { ...port, default: seedValue(port) },
                });
                setInput('');
                setError('');
              }}
            >
              <Plus size={13} />
              {t('editor.addWorkflowInput')}
            </button>
          </div>
        </section>
        <section className="port-editor">
          <h3>{t('editor.workflowOutputs')}</h3>
          <p className="setup-hint">{t('editor.theResultsReturnedWhenThisGraphFinishes')}</p>
          {Object.entries(graph.outputs ?? {}).map(([name, port]) => (
            <div className="port-editor-card" key={name}>
              <div className="inline spread">
                <strong>{name}</strong>
                <button
                  className="icon-button"
                  disabled={Object.keys(graph.outputs ?? {}).length <= 1}
                  aria-label={t('editor.removeWorkflowOutputName', { name })}
                  onClick={() => {
                    const next = { ...graph.outputs };
                    delete next[name];
                    onGraphEdit('outputs', next);
                  }}
                >
                  <Trash2 size={13} />
                </button>
              </div>
              <BindingEditor
                name={name}
                port={port}
                graph={graph}
                imports={imports}
                onChange={(bind) =>
                  onGraphEdit('outputs', { ...graph.outputs, [name]: { ...port, bind } })
                }
              />
            </div>
          ))}
          <div className="port-add-form">
            <label className="field">
              {t('editor.newWorkflowOutput')}
              <input
                value={output}
                onChange={(event) => setOutput(event.target.value)}
                placeholder="result"
              />
            </label>
            <label className="field">
              {t('editor.exportFrom')}
              <select value={source} onChange={(event) => setSource(event.target.value)}>
                <option value="">{t('editor.chooseAResult')}</option>
                {options.map((option) => (
                  <option key={option.value} value={option.value}>
                    {option.node
                      ? option.label
                      : t('editor.workflowInputName', { name: option.name })}
                  </option>
                ))}
              </select>
            </label>
            <button
              className="button small-button"
              onClick={() => {
                const origin = options.find((option) => option.value === source);
                if (!validName(output, graph.outputs ?? {}) || !origin) {
                  setError('Choose a unique output name and a source.');
                  return;
                }
                const { schema, schemaRef, artifact } = origin.port;
                onGraphEdit('outputs', {
                  ...graph.outputs,
                  [output]: {
                    ...(schema !== undefined ? { schema } : {}),
                    ...(schemaRef ? { schemaRef } : {}),
                    ...(artifact ? { artifact } : {}),
                    bind: { from: source },
                  },
                });
                setOutput('');
                setError('');
              }}
            >
              <Plus size={13} />
              {t('editor.addWorkflowOutput')}
            </button>
          </div>
        </section>
        {!scope.length ? (
          <section className="workflow-resources" ref={resourcePanel}>
            <h3>{t('navigation.resources')}</h3>
            <p className="setup-hint">{t('editor.aliasesUsedByBlocksConnectionAndProfileIds')}</p>
            {(['models', 'mcp', 'sandboxes'] as const).map((kind) => (
              <ResourceList
                initiallyOpen={showResources}
                key={kind}
                kind={kind}
                resources={pipeline.spec[kind] ?? {}}
                onSave={(value) => onResources(kind, value)}
              />
            ))}
          </section>
        ) : null}
        {error ? (
          <p className="form-error" role="alert">
            {message(error)}
          </p>
        ) : null}
      </div>
    </aside>
  );
}

function ResourceList({
  kind,
  resources,
  onSave,
  initiallyOpen,
}: {
  initiallyOpen?: boolean;
  kind: 'models' | 'mcp' | 'sandboxes';
  resources: Record<string, Record<string, unknown>>;
  onSave: (value: Record<string, Record<string, unknown>>) => void;
}) {
  const { t, message } = useI18n();
  const [expanded, setExpanded] = useState(!!initiallyOpen);
  const [alias, setAlias] = useState('');
  const [connection, setConnection] = useState('');
  const [error, setError] = useState('');
  const field = kind === 'sandboxes' ? 'profile' : 'connection';
  return (
    <details
      className="field-details"
      open={expanded}
      onToggle={(event) => setExpanded(event.currentTarget.open)}
    >
      <summary>
        {t(
          kind === 'mcp'
            ? 'editor.mcpServers'
            : kind === 'sandboxes'
              ? 'resources.sandboxes'
              : 'resources.models',
        )}{' '}
        ({Object.keys(resources).length})
      </summary>
      {Object.entries(resources).map(([name, resource]) => (
        <label className="field" key={name}>
          {name} · {t(field === 'profile' ? 'editor.profileId' : 'editor.connectionId')}
          <input
            aria-label={`${name} · ${t(field === 'profile' ? 'editor.profileId' : 'editor.connectionId')}`}
            defaultValue={String(resource[field])}
            key={String(resource[field])}
            onBlur={(event) => {
              if (event.target.value.trim())
                onSave({ ...resources, [name]: { ...resource, [field]: event.target.value } });
            }}
          />
        </label>
      ))}
      <label className="field">
        {t(
          kind === 'models'
            ? 'editor.newModelAlias'
            : kind === 'mcp'
              ? 'editor.newServerAlias'
              : 'editor.newSandboxAlias',
        )}
        <input
          value={alias}
          onChange={(event) => setAlias(event.target.value)}
          placeholder={t('editor.eGResearcher')}
        />
      </label>
      <label className="field">
        {t(field === 'profile' ? 'editor.profileId2' : 'editor.connectionId2')}
        <input
          value={connection}
          onChange={(event) => setConnection(event.target.value)}
          placeholder={t('editor.idInTheTrustedEngineProfile')}
        />
      </label>
      <button
        className="button small-button"
        onClick={() => {
          if (
            !/^[a-z][a-z0-9_]{0,63}$/.test(alias) ||
            resources[alias] ||
            !/^[a-z][a-z0-9_]{0,63}$/.test(connection)
          ) {
            setError('Use unique lowercase identifiers for the alias and engine ID.');
            return;
          }
          onSave({ ...resources, [alias]: { [field]: connection } });
          setAlias('');
          setConnection('');
          setError('');
        }}
      >
        <Plus size={13} />
        {t('editor.addAlias')}
      </button>
      {error ? (
        <p className="form-error" role="alert">
          {message(error)}
        </p>
      ) : null}
    </details>
  );
}
