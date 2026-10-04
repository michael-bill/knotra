import { useI18n } from '../lib/i18n';
import { useEffect, useState } from 'react';
import { Copy, Trash2, Braces, ArrowRight, Settings2, Plus, X } from 'lucide-react';
import { nodePorts } from '../lib/graph';
import { record, type Graph, type NodeDefinition, type Pipeline } from '../lib/types';
import PortEditor, { JsonField } from './PortEditor';
import { NodeIcon, nodeMeta } from './ui';

function TextField({
  label,
  value,
  onSave,
  hint,
  multiline = false,
  optional = false,
}: {
  label: string;
  value: string;
  onSave: (value: string) => void;
  hint?: string;
  multiline?: boolean;
  optional?: boolean;
}) {
  const { message } = useI18n();
  const [error, setError] = useState('');
  const props = {
    'aria-label': label,
    defaultValue: value,
    onBlur: (event: React.FocusEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      if (!optional && !event.target.value.trim()) {
        setError('This field cannot be empty.');
        return;
      }
      setError('');
      if (event.target.value !== value) onSave(event.target.value);
    },
  };
  return (
    <label className="field">
      {label}
      {multiline ? <textarea key={value} rows={5} {...props} /> : <input key={value} {...props} />}
      {hint ? <small>{hint}</small> : null}
      {error ? (
        <span className="form-error" role="alert">
          {message(error)}
        </span>
      ) : null}
    </label>
  );
}

function NumberField({
  label,
  value,
  max,
  onSave,
  hint,
}: {
  label: string;
  value: number;
  max: number;
  onSave: (value: number) => void;
  hint: string;
}) {
  const { message } = useI18n();
  const [error, setError] = useState('');
  return (
    <label className="field">
      {label}
      <input
        key={String(value)}
        type="number"
        aria-label={label}
        min={1}
        max={max}
        step={1}
        defaultValue={value}
        onBlur={(event) => {
          const n = Number(event.target.value);
          if (!Number.isInteger(n) || n < 1 || n > max) {
            setError('Enter a whole number from 1 to {max}.');
            return;
          }
          setError('');
          if (n !== value) onSave(n);
        }}
      />
      <small>{hint}</small>
      {error ? (
        <span className="form-error" role="alert">
          {message(error, { max })}
        </span>
      ) : null}
    </label>
  );
}

export default function Inspector({
  id,
  node,
  graph,
  resources,
  files,
  onEdit,
  onSource,
  onDuplicate,
  onRemove,
  onBody,
  importedGraphs,
  onWorkflow,
  showPorts,
}: {
  id?: string;
  node?: NodeDefinition;
  graph: Graph;
  resources: Pipeline['spec'];
  files: string[];
  importedGraphs?: Map<string, Graph>;
  onEdit: (key: string, value: unknown) => void;
  onSource: () => void;
  onDuplicate: () => void;
  onRemove: () => void;
  onBody: () => void;
  onWorkflow?: () => void;
  showPorts?: boolean;
  onNotify?: (message: string) => void;
}) {
  const { t } = useI18n();
  const [tab, setTab] = useState<'setup' | 'ports'>('setup');
  useEffect(() => {
    if (showPorts) setTab('ports');
  }, [showPorts]);
  if (!node || !id) return null;
  const meta = nodeMeta[node.type];
  const config = record(node[node.type]);
  const prompt = record(config.prompt);
  const outputs = nodePorts(node, importedGraphs);
  const editConfig = (key: string, value: unknown) =>
    onEdit(node.type, { ...config, [key]: value });
  const aliases = (kind: 'models' | 'mcp' | 'sandboxes') => Object.keys(resources[kind] ?? {});
  const resourceSelect = (
    label: string,
    kind: 'models' | 'mcp' | 'sandboxes',
    value: string,
    onSave: (value: string) => void,
  ) => (
    <div className="resource-field">
      <label className="field">
        {label}
        <select aria-label={label} value={value} onChange={(event) => onSave(event.target.value)}>
          {!aliases(kind).includes(value) ? (
            <option value={value}>{value || t('editor.chooseAnAlias')}</option>
          ) : null}
          {aliases(kind).map((alias) => (
            <option key={alias}>{alias}</option>
          ))}
        </select>
        <small>{t('editor.usesAResourceDeclaredInThisWorkflow')}</small>
      </label>
      <button className="text-button" type="button" onClick={onWorkflow}>
        {t(
          kind === 'models'
            ? 'editor.manageModels'
            : kind === 'mcp'
              ? 'editor.manageMcpServers'
              : 'editor.manageSandboxes',
        )}
        <ArrowRight size={12} />
      </button>
    </div>
  );
  return (
    <aside className="inspector setup-inspector">
      <header className="inspector-header">
        <span className="eyebrow">{t('editor.blockSettings')}</span>
        <div className="inline">
          <button
            className="icon-button"
            onClick={onDuplicate}
            aria-label={t('editor.duplicateNode')}
          >
            <Copy size={15} />
          </button>
          <button
            className="icon-button"
            onClick={onRemove}
            disabled={Object.keys(graph.nodes).length <= 1}
            title={
              Object.keys(graph.nodes).length <= 1
                ? t('editor.keepAtLeastOneBlockInTheWorkflow')
                : t('editor.deleteBlock')
            }
            aria-label={t('editor.deleteNode')}
          >
            <Trash2 size={15} />
          </button>
        </div>
      </header>
      <div className="inspector-title">
        <span className={`node-icon tint-${meta.color}`}>
          <NodeIcon kind={node.type} size={20} />
        </span>
        <div>
          <h2>{id.replace(/_/g, ' ')}</h2>
          <p>{t(meta.detail)}</p>
        </div>
      </div>
      <div className="inspector-tabs">
        <button className={tab === 'setup' ? 'active' : ''} onClick={() => setTab('setup')}>
          <Settings2 size={13} />
          {t('editor.setup')}
        </button>
        <button className={tab === 'ports' ? 'active' : ''} onClick={() => setTab('ports')}>
          {t('editor.inputsOutputs')}
        </button>
      </div>
      <div className="inspector-content">
        {tab === 'setup' ? (
          <>
            <TextField
              label={t('editor.description')}
              value={node.description ?? ''}
              optional
              onSave={(value) => onEdit('description', value || undefined)}
            />
            {['llm', 'agent'].includes(node.type)
              ? resourceSelect(
                  t('editor.modelAlias'),
                  'models',
                  String(config.model ?? ''),
                  (value) => editConfig('model', value),
                )
              : null}
            {['agent', 'code'].includes(node.type)
              ? resourceSelect(t('editor.sandbox'), 'sandboxes', node.sandbox ?? '', (value) =>
                  onEdit('sandbox', value),
                )
              : null}
            {['llm', 'agent', 'human'].includes(node.type) ? (
              <>
                <label className="field">
                  {t('editor.promptSource')}
                  <select
                    value={prompt.file ? 'file' : 'text'}
                    onChange={(event) => {
                      if (event.target.value === 'text')
                        editConfig('prompt', { text: 'Describe the task to complete.' });
                      else {
                        const file = files.find((file) => !/\.ya?ml$/.test(file));
                        if (file) editConfig('prompt', { file });
                      }
                    }}
                  >
                    <option value="text">{t('editor.writeInstructionsHere')}</option>
                    <option value="file" disabled={!files.some((file) => !/\.ya?ml$/.test(file))}>
                      {t('editor.packageFile')}
                    </option>
                  </select>
                </label>
                {prompt.file ? (
                  <label className="field">
                    {t('editor.promptFile')}
                    <select
                      value={String(prompt.file)}
                      onChange={(event) => editConfig('prompt', { file: event.target.value })}
                    >
                      {files.map((file) => (
                        <option key={file}>{file}</option>
                      ))}
                    </select>
                  </label>
                ) : (
                  <TextField
                    label={t(
                      node.type === 'human' ? 'execution.reviewRequest' : 'editor.instructions',
                    )}
                    multiline
                    value={String(prompt.text ?? '')}
                    onSave={(text) => editConfig('prompt', { text })}
                    hint={t('editor.inputsArriveAsSeparateDataDescribeTheTask')}
                  />
                )}
              </>
            ) : null}
            {node.type === 'agent' ? (
              <>
                <NumberField
                  label={t('editor.maximumModelSteps')}
                  value={Number(config.maxSteps)}
                  max={10000}
                  onSave={(value) => editConfig('maxSteps', value)}
                  hint={t('editor.limitsHowManyModelCallsThisAgentCan')}
                />
                <details className="field-details">
                  <summary>{t('editor.toolPermissions')}</summary>
                  <JsonField
                    label={t('editor.allowedTools')}
                    value={node.tools ?? { inherit: true }}
                    validate={(value) =>
                      !!value && typeof value === 'object' && !Array.isArray(value)
                    }
                    onSave={(value) => onEdit('tools', value)}
                    hint={t('editor.inheritMcpAliasesWithToolListsAndSandbox')}
                  />
                </details>
              </>
            ) : null}
            {node.type === 'code' ? (
              <div className="command-editor">
                <TextField
                  label={t('editor.executable')}
                  value={String((config.command as string[])[0])}
                  onSave={(value) =>
                    editConfig('command', [value, ...(config.command as string[]).slice(1)])
                  }
                  hint={t('editor.theEngineRunsThisExecutableInTheSelected')}
                />
                <h3>{t('editor.arguments')}</h3>
                {(config.command as string[]).slice(1).map((argument, index) => (
                  <div className="command-argument" key={index}>
                    <TextField
                      label={t('editor.argumentCount', { count: index + 1 })}
                      value={argument}
                      optional
                      multiline={argument.includes('\n') || argument.length > 80}
                      onSave={(value) =>
                        editConfig(
                          'command',
                          (config.command as string[]).map((arg, i) =>
                            i === index + 1 ? value : arg,
                          ),
                        )
                      }
                    />
                    <button
                      className="icon-button"
                      aria-label={t('editor.removeArgumentCount', { count: index + 1 })}
                      onClick={() =>
                        editConfig(
                          'command',
                          (config.command as string[]).filter((_, i) => i !== index + 1),
                        )
                      }
                    >
                      <X size={13} />
                    </button>
                  </div>
                ))}
                <button
                  className="text-button"
                  onClick={() => editConfig('command', [...(config.command as string[]), ''])}
                >
                  <Plus size={13} />
                  {t('editor.addArgument')}
                </button>
              </div>
            ) : null}
            {node.type === 'tool' ? (
              <>
                {resourceSelect(t('editor.mcpServerAlias'), 'mcp', String(config.server), (value) =>
                  editConfig('server', value),
                )}
                <TextField
                  label={t('editor.toolName')}
                  value={String(config.name)}
                  onSave={(value) => editConfig('name', value)}
                  hint={t('editor.exactToolNameExposedByThisMcpServer')}
                />
                <label className="field">
                  {t('editor.toolArguments')}
                  <select
                    value={'expr' in record(config.arguments) ? 'expr' : 'value'}
                    onChange={(event) =>
                      editConfig(
                        'arguments',
                        event.target.value === 'expr' ? { expr: 'args' } : { value: {} },
                      )
                    }
                  >
                    <option value="value">{t('editor.fixedJsonObject')}</option>
                    <option value="expr">{t('editor.useBlockInputsCel')}</option>
                  </select>
                </label>
                {'expr' in record(config.arguments) ? (
                  <TextField
                    label={t('editor.argumentsExpression')}
                    value={String(record(config.arguments).expr)}
                    onSave={(expr) => editConfig('arguments', { expr })}
                    hint={t('editor.argsContainsThisBlockSInputsReturnAn')}
                  />
                ) : (
                  <JsonField
                    label={t('editor.argumentsJson')}
                    value={record(config.arguments).value ?? config.arguments}
                    onSave={(value) => editConfig('arguments', { value })}
                    validate={(value) =>
                      !!value && typeof value === 'object' && !Array.isArray(value)
                    }
                    hint={t('editor.aJsonObjectWithTheToolSArgument')}
                  />
                )}
                <label className="field">
                  {t('editor.responseFormat')}
                  <select
                    value={String(config.response ?? 'structured')}
                    onChange={(event) => editConfig('response', event.target.value)}
                  >
                    <option value="structured">{t('editor.structuredData')}</option>
                    <option value="content">{t('editor.contentBlocks')}</option>
                  </select>
                </label>
              </>
            ) : null}
            {node.type === 'switch' ? (
              <>
                <p className="setup-hint">
                  {t('editor.firstMatchingConditionWinsLaterBlocksCanRead')}
                </p>
                {(config.cases as { name: string; when: string }[]).map((item, index) => (
                  <div className="setup-group" key={index}>
                    <TextField
                      label={t('editor.routeCountName', { count: index + 1 })}
                      value={item.name}
                      onSave={(name) =>
                        editConfig(
                          'cases',
                          (config.cases as any[]).map((item, i) =>
                            i === index ? { ...item, name } : item,
                          ),
                        )
                      }
                    />
                    <TextField
                      label={t('editor.routeCountCondition', { count: index + 1 })}
                      value={item.when}
                      onSave={(when) =>
                        editConfig(
                          'cases',
                          (config.cases as any[]).map((item, i) =>
                            i === index ? { ...item, when } : item,
                          ),
                        )
                      }
                      hint={t('editor.aCelBooleanUsingArgsEGArgs')}
                    />
                    <button
                      className="text-button"
                      disabled={(config.cases as any[]).length <= 1}
                      onClick={() =>
                        editConfig(
                          'cases',
                          (config.cases as any[]).filter((_, i) => i !== index),
                        )
                      }
                    >
                      {t('editor.removeRoute')}
                    </button>
                  </div>
                ))}
                <button
                  className="text-button"
                  onClick={() => {
                    const cases = config.cases as { name: string; when: string }[];
                    let i = cases.length + 1;
                    while (
                      cases.some((item) => item.name === `route_${i}`) ||
                      config.default === `route_${i}`
                    )
                      i++;
                    editConfig('cases', [...cases, { name: `route_${i}`, when: 'false' }]);
                  }}
                >
                  <Plus size={13} />
                  {t('editor.addRoute')}
                </button>
                <TextField
                  label={t('editor.defaultRoute')}
                  value={String(config.default)}
                  onSave={(value) => editConfig('default', value)}
                />
              </>
            ) : null}
            {node.type === 'foreach' ? (
              <>
                <label className="field">
                  {t('editor.collectionInput')}
                  <select
                    value={String(config.over)}
                    onChange={(event) => editConfig('over', event.target.value)}
                  >
                    {Object.entries(node.inputs ?? {})
                      .filter(
                        ([, port]) =>
                          record(port.schema).type === 'array' || port.artifact?.collection,
                      )
                      .map(([name]) => (
                        <option key={name}>{name}</option>
                      ))}
                  </select>
                </label>
                <NumberField
                  label={t('editor.concurrentIterations')}
                  value={Number(config.concurrency)}
                  max={2147483647}
                  onSave={(value) => editConfig('concurrency', value)}
                  hint={t('editor.howManyItemsMayBeProcessedAtOnce')}
                />
              </>
            ) : null}
            {node.type === 'loop' ? (
              <>
                <NumberField
                  label={t('editor.maximumIterations')}
                  value={Number(config.maxIterations)}
                  max={100000}
                  onSave={(value) => editConfig('maxIterations', value)}
                  hint={t('editor.aHardLimitOnRepeatedBodyExecution')}
                />
                <TextField
                  label={t('editor.stopCondition')}
                  value={String(config.until)}
                  onSave={(value) => editConfig('until', value)}
                  hint={t('editor.aCelBooleanEvaluatedAfterTheBodyFinishes')}
                />
                <label className="field">
                  {t('editor.whenTheLimitIsReached')}
                  <select
                    value={String(config.onLimit)}
                    onChange={(event) => editConfig('onLimit', event.target.value)}
                  >
                    <option value="fail">{t('editor.failTheRun')}</option>
                    <option value="return_last">{t('editor.returnTheLastResult')}</option>
                  </select>
                </label>
                <details className="field-details">
                  <summary>{t('editor.stateCarriedBetweenIterations')}</summary>
                  <JsonField
                    label={t('editor.loopState')}
                    value={config.state}
                    onSave={(value) => editConfig('state', value)}
                    validate={(value) =>
                      !!value && typeof value === 'object' && !Array.isArray(value)
                    }
                    hint={t('editor.eachStateValueDeclaresSchemaInitialBindingAnd')}
                  />
                </details>
              </>
            ) : null}
            {['foreach', 'loop'].includes(node.type) ? (
              <>
                <button className="button full" onClick={onBody}>
                  {t('editor.editBodyGraph')}
                  <ArrowRight size={15} />
                </button>
                <details className="field-details">
                  <summary>{t('editor.inputsPassedToTheBody')}</summary>
                  <JsonField
                    label={t('editor.bodyInputBindings')}
                    value={config.with}
                    onSave={(value) => editConfig('with', value)}
                    validate={(value) =>
                      !!value && typeof value === 'object' && !Array.isArray(value)
                    }
                    hint={
                      node.type === 'foreach'
                        ? t('editor.bindBodyInputNamesToArgsIterationItem')
                        : t('editor.bindBodyInputNamesToArgsStateOr')
                    }
                  />
                </details>
              </>
            ) : null}
            {node.type === 'pipeline' ? (
              <>
                <label className="field">
                  {t('editor.childPipelineFile')}
                  <select
                    aria-label={t('editor.childPipelineFile')}
                    value={String(config.file)}
                    onChange={(event) => editConfig('file', event.target.value)}
                  >
                    {files
                      .filter((file) => /\.ya?ml$/.test(file))
                      .map((file) => (
                        <option key={file}>{file}</option>
                      ))}
                  </select>
                  <small>{t('editor.addTheChildPackageInFilesFirst')}</small>
                </label>
                <JsonField
                  label={t('editor.childPermissions')}
                  value={config.permissions}
                  onSave={(value) => editConfig('permissions', value)}
                  validate={(value) =>
                    !!value && typeof value === 'object' && !Array.isArray(value)
                  }
                  hint={t('editor.explicitModelMcpSandboxAndSecretPermissionsFor')}
                />
              </>
            ) : null}
            <button className="button full setup-next" onClick={() => setTab('ports')}>
              {t('editor.connectInputsDefineOutputs')}
              <ArrowRight size={15} />
            </button>
            <details className="field-details">
              <summary>{t('editor.runConditionsAdvancedOptions')}</summary>
              <TextField
                label={t('editor.runConditionCel')}
                value={node.when ?? ''}
                optional
                onSave={(value) => onEdit('when', value || undefined)}
                hint={t('editor.leaveBlankToRunWheneverInputsAreReady')}
              />
              <TextField
                label={t('editor.waitForBlocks')}
                value={(node.needs ?? []).join(', ')}
                optional
                onSave={(value) =>
                  onEdit(
                    'needs',
                    value
                      ? value
                          .split(',')
                          .map((value) => value.trim())
                          .filter(Boolean)
                      : undefined,
                  )
                }
                hint={t('editor.commaSeparatedBlockIdsThisWaitsWithoutTransferring')}
              />
              <button className="text-button" onClick={onSource}>
                <Braces size={14} />
                {t('editor.openYaml')}
              </button>
            </details>
          </>
        ) : (
          <>
            <PortEditor
              key={id + 'inputs'}
              ports={node.inputs ?? {}}
              direction="inputs"
              graph={graph}
              imports={importedGraphs}
              nodeId={id}
              onChange={(inputs) => onEdit('inputs', inputs)}
              allowFiles={node.type !== 'llm'}
              schemas={Object.keys(resources.schemas ?? {})}
            />
            <PortEditor
              key={id + 'outputs'}
              ports={outputs}
              direction="outputs"
              graph={graph}
              imports={importedGraphs}
              onChange={(outputs) => onEdit('outputs', outputs)}
              editable={!['switch', 'foreach', 'loop', 'pipeline'].includes(node.type)}
              minimum={1}
              canAdd={node.type !== 'tool'}
              allowFiles={['agent', 'code'].includes(node.type)}
              schemas={Object.keys(resources.schemas ?? {})}
            />
            {['switch', 'foreach', 'loop', 'pipeline'].includes(node.type) ? (
              <p className="setup-hint">
                {t(
                  node.type === 'pipeline'
                    ? 'editor.theseOutputsComeFromTheChildPipelineExports'
                    : node.type === 'switch'
                      ? 'editor.theseOutputsComeFromTheSelectedRouteEdit'
                      : 'editor.theseOutputsComeFromTheBodyGraphExports',
                )}
              </p>
            ) : null}
          </>
        )}
      </div>
    </aside>
  );
}
