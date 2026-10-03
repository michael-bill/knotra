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
        <span className="eyebrow">{scope.length ? 'BODY GRAPH' : 'WORKFLOW SETTINGS'}</span>
        <Settings2 size={16} />
      </header>
      <div className="inspector-content">
        {!scope.length ? (
          <label className="field">
            Workflow title
            <input
              aria-label="Workflow title"
              defaultValue={pipeline.metadata.title ?? pipeline.metadata.name}
              key={pipeline.metadata.title}
              onBlur={(event) => {
                if (event.target.value.trim()) onMetadata(event.target.value);
              }}
            />
          </label>
        ) : (
          <p className="setup-hint">
            Inputs and exports belong to this body graph. Use the parent block’s body bindings to
            pass values in.
          </p>
        )}
        <section className="port-editor">
          <h3>Workflow inputs</h3>
          <p className="setup-hint">
            Starting values for a run. Blocks can choose these as their input sources.
          </p>
          {Object.entries(graph.inputs ?? {}).map(([name, port]) => (
            <div className="port-editor-card" key={name}>
              <div className="inline spread">
                <strong>{name}</strong>
                <button
                  className="icon-button"
                  aria-label={`Remove workflow input ${name}`}
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
                <small>File · {port.artifact.mediaTypes.join(', ')}</small>
              ) : (
                <>
                  <label className="field">
                    Input type
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
                          Custom schema{port.schemaRef ? ` · ${port.schemaRef}` : ''}
                        </option>
                      ) : null}
                      {['string', 'object', 'array', 'integer', 'number', 'boolean', 'JSON'].map(
                        (type) => (
                          <option key={type}>{type}</option>
                        ),
                      )}
                    </select>
                  </label>
                  {'default' in port ? (
                    <JsonField
                      label={`Default for ${name}`}
                      value={port.default}
                      onSave={(value) =>
                        onGraphEdit('inputs', {
                          ...graph.inputs,
                          [name]: { ...port, default: value },
                        })
                      }
                      hint="JSON value; strings use quotes."
                    />
                  ) : null}
                </>
              )}
              <details className="field-details">
                <summary>Schema & details</summary>
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
              New workflow input
              <input
                value={input}
                onChange={(event) => setInput(event.target.value)}
                placeholder="topic"
              />
            </label>
            <label className="field">
              Data type
              <select value={type} onChange={(event) => setType(event.target.value)}>
                {['string', 'object', 'array', 'integer', 'number', 'boolean'].map((type) => (
                  <option key={type}>{type}</option>
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
              Add workflow input
            </button>
          </div>
        </section>
        <section className="port-editor">
          <h3>Workflow outputs</h3>
          <p className="setup-hint">The results returned when this graph finishes.</p>
          {Object.entries(graph.outputs ?? {}).map(([name, port]) => (
            <div className="port-editor-card" key={name}>
              <div className="inline spread">
                <strong>{name}</strong>
                <button
                  className="icon-button"
                  disabled={Object.keys(graph.outputs ?? {}).length <= 1}
                  aria-label={`Remove workflow output ${name}`}
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
              New workflow output
              <input
                value={output}
                onChange={(event) => setOutput(event.target.value)}
                placeholder="result"
              />
            </label>
            <label className="field">
              Export from
              <select value={source} onChange={(event) => setSource(event.target.value)}>
                <option value="">Choose a result</option>
                {options.map((option) => (
                  <option key={option.value} value={option.value}>
                    {option.label}
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
              Add workflow output
            </button>
          </div>
        </section>
        {!scope.length ? (
          <section className="workflow-resources" ref={resourcePanel}>
            <h3>Resources</h3>
            <p className="setup-hint">
              Aliases used by blocks. Connection and profile IDs must match your engine profile.
            </p>
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
            {error}
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
        {kind === 'mcp' ? 'MCP servers' : kind === 'sandboxes' ? 'Sandboxes' : 'Models'} (
        {Object.keys(resources).length})
      </summary>
      {Object.entries(resources).map(([name, resource]) => (
        <label className="field" key={name}>
          {name} · {field} ID
          <input
            aria-label={`${name} · ${field} ID`}
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
        New {kind === 'models' ? 'model' : kind === 'mcp' ? 'server' : 'sandbox'} alias
        <input
          value={alias}
          onChange={(event) => setAlias(event.target.value)}
          placeholder="e.g. researcher"
        />
      </label>
      <label className="field">
        {field === 'profile' ? 'Profile' : 'Connection'} ID
        <input
          value={connection}
          onChange={(event) => setConnection(event.target.value)}
          placeholder="ID in the trusted engine profile"
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
        Add alias
      </button>
      {error ? (
        <p className="form-error" role="alert">
          {error}
        </p>
      ) : null}
    </details>
  );
}
