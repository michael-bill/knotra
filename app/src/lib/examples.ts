import { parseDocument, stringify } from 'yaml';
import localSource from '../../../examples/local/pipeline.yaml?raw';
import { base64, textBytes } from './bytes';
import type { PackageFile, Pipeline, Workspace } from './types';
import { translate, type Locale, type MessageKey } from './i18n';

const stringPort = { schema: { type: 'string' } };
export const researchPipeline: Pipeline = {
  apiVersion: 'knotra/v1',
  kind: 'Pipeline',
  metadata: {
    name: 'research-brief',
    title: 'Research brief',
    description: 'From a question to a reviewed, shareable brief.',
    version: '1.0.0',
  },
  spec: {
    models: {
      researcher: { connection: 'model_main', requires: ['toolCalling'] },
      writer: { connection: 'model_main', requires: ['structuredOutput'] },
    },
    mcp: { search: { connection: 'search_remote' } },
    sandboxes: { work: { profile: 'python_box' } },
    inputs: {
      topic: {
        ...stringPort,
        description: 'What would you like to explore?',
        default: 'Reliable AI workflows',
      },
    },
    nodes: {
      discover: {
        type: 'tool',
        description: 'Gather source material',
        inputs: { topic: { ...stringPort, bind: { from: 'inputs.topic' } } },
        tool: {
          server: 'search',
          name: 'search',
          arguments: { expr: '{"query": args.topic}' },
          response: 'structured',
        },
        outputs: { result: { schema: { type: 'object', additionalProperties: true } } },
      },
      research: {
        type: 'agent',
        description: 'Investigate the question',
        sandbox: 'work',
        inputs: {
          sources: { schema: { type: 'object' }, bind: { from: 'nodes.discover.outputs.result' } },
        },
        tools: {
          inherit: false,
          mcp: { search: ['search'] },
          sandbox: ['files.read', 'files.write', 'process.exec'],
        },
        agent: {
          model: 'researcher',
          prompt: {
            text: 'Investigate the supplied source material. Return a concise evidence-based summary.',
          },
          maxSteps: 12,
        },
        outputs: { summary: stringPort },
      },
      draft: {
        type: 'llm',
        description: 'Write a clear, concise brief',
        inputs: { summary: { ...stringPort, bind: { from: 'nodes.research.outputs.summary' } } },
        llm: {
          model: 'writer',
          prompt: {
            text: 'Create a Markdown research brief from the summary. Include findings and open questions.',
          },
        },
        outputs: { brief: stringPort },
      },
      review: {
        type: 'human',
        description: 'Give the brief a final review',
        inputs: { brief: { ...stringPort, bind: { from: 'nodes.draft.outputs.brief' } } },
        human: {
          prompt: {
            text: 'Review the brief and return your feedback. Your response is saved with this run.',
          },
        },
        outputs: { feedback: stringPort },
      },
      publish: {
        type: 'code',
        description: 'Package the reviewed brief',
        sandbox: 'work',
        inputs: {
          brief: { ...stringPort, bind: { from: 'nodes.draft.outputs.brief' } },
          feedback: { ...stringPort, bind: { from: 'nodes.review.outputs.feedback' } },
        },
        code: {
          command: [
            'python3',
            '-c',
            'import json, os; from pathlib import Path; args=json.loads(Path(os.environ["KNOTRA_INPUT_JSON"]).read_text())["values"]; Path("brief.md").write_text(args["brief"]+"\\n\\nReview: "+args["feedback"]); Path(os.environ["KNOTRA_OUTPUT_JSON"]).write_text("{}")',
          ],
        },
        outputs: {
          document: {
            artifact: { mediaTypes: ['text/markdown'] },
            collect: { path: 'brief.md', mediaType: 'text/markdown' },
          },
        },
      },
    },
    outputs: {
      document: {
        artifact: { mediaTypes: ['text/markdown'] },
        bind: { from: 'nodes.publish.outputs.document' },
      },
    },
  },
};
export const RESEARCH_SOURCE =
  '# Knotra research workflow · v1\n# Resources resolve through your engine profile.\n' +
  stringify(researchPipeline, { lineWidth: 110, aliasDuplicateObjects: false });

export interface Example {
  id: string;
  titleKey: MessageKey;
  descriptionKey: MessageKey;
  kindKey: MessageKey;
  category: 'starter' | 'block' | 'demo';
  requirements: ('model' | 'sandbox' | 'nodeSandbox' | 'mcp' | 'secret')[];
  resultKey?: MessageKey;
  entrypoint?: string;
  source: string;
  files: PackageFile[];
}

const rawFiles = import.meta.glob('../../../contracts/v1/fixtures/positive/**/*', {
  eager: true,
  query: '?raw',
  import: 'default',
}) as Record<string, string>;
const starterFiles = import.meta.glob('../../../examples/starter/*/**/*', {
  eager: true,
  query: '?raw',
  import: 'default',
}) as Record<string, string>;

function packageFiles(
  files: Record<string, string>,
  prefix: string,
  entrypoint = 'pipeline.yaml',
): PackageFile[] {
  return Object.entries(files)
    .filter(([path]) => path.startsWith(prefix) && path !== prefix + entrypoint)
    .map(([path, content]) => ({
      path: path.slice(prefix.length),
      content: base64(textBytes(content)),
    }));
}

const starters: (Pick<
  Example,
  'id' | 'titleKey' | 'descriptionKey' | 'kindKey' | 'resultKey' | 'entrypoint'
> & { directory?: string })[] = [
  {
    id: 'hello',
    titleKey: 'starter.hello.title',
    descriptionKey: 'starter.hello.description',
    kindKey: 'starter.hello.kind',
    resultKey: 'starter.hello.result',
  },
  {
    id: 'research-dossier',
    titleKey: 'starter.research.title',
    descriptionKey: 'starter.research.description',
    kindKey: 'starter.research.kind',
    resultKey: 'starter.research.result',
  },
  {
    id: 'reviewed-research',
    directory: 'research-dossier',
    entrypoint: 'review.yaml',
    titleKey: 'starter.reviewed.title',
    descriptionKey: 'starter.reviewed.description',
    kindKey: 'starter.reviewed.kind',
    resultKey: 'starter.reviewed.result',
  },
  {
    id: 'tic-tac-toe',
    titleKey: 'starter.game.title',
    descriptionKey: 'starter.game.description',
    kindKey: 'starter.game.kind',
    resultKey: 'starter.game.result',
  },
  {
    id: 'publication',
    titleKey: 'starter.publication.title',
    descriptionKey: 'starter.publication.description',
    kindKey: 'starter.publication.kind',
    resultKey: 'starter.publication.result',
  },
];

const fixtureRequirements: Record<string, Example['requirements']> = {
  llm: ['model'],
  agent: ['model', 'sandbox', 'mcp', 'secret'],
  code: ['sandbox'],
  tool: ['mcp'],
  switch: [],
  human: [],
  foreach: [],
  loop: [],
  subpipeline: ['model'],
  'artifact-mount': ['sandbox'],
};
const fixtureTitles: Record<string, [MessageKey, MessageKey]> = {
  llm: ['shell.modelResponse', 'shell.structuredGenerationWithAPromptAndAnExternal'],
  agent: ['shell.autonomousResearch', 'shell.aBoundedAgentWithMcpToolsAndAn'],
  code: ['shell.createAnArtifact', 'shell.runACommandAndCollectADeclaredOutput'],
  tool: ['shell.mcpToolCall', 'shell.callASpecificToolWithAStructuredResponse'],
  switch: ['editor.conditionalRouting', 'shell.selectABranchAndCoalesceTheResultingOutputs'],
  human: ['execution.humanReview', 'shell.waitForAResponseThatMatchesTheOutput'],
  foreach: ['shell.parallelReview', 'shell.reviewACollectionWithIsolatedConcurrentIterations'],
  loop: ['shell.iterativeRefinement', 'shell.carryStateThroughABoundedLoop'],
  subpipeline: ['shell.reusablePipeline', 'shell.invokeAChildPackageWithExplicitPermissions'],
  'artifact-mount': ['shell.passAFile', 'shell.mountADeclaredArtifactInASeparateSandbox'],
};
export const examples: Example[] = [
  {
    id: 'research',
    titleKey: 'shell.researchBrief',
    descriptionKey: 'shell.exploreTheCompleteWorkbenchWithAGuidedDemo',
    kindKey: 'shell.5NodesDemoAvailable',
    category: 'demo',
    requirements: ['model', 'sandbox', 'mcp'],
    source: RESEARCH_SOURCE,
    files: [],
  },
  {
    id: 'local',
    titleKey: 'library.local.title',
    descriptionKey: 'shell.generateAndSaveAGreetingWithTheBundled',
    kindKey: 'shell.llmCodeLocalProfile',
    category: 'block',
    requirements: ['model', 'sandbox'],
    source: localSource,
    files: [],
  },
  ...starters.map(({ directory, ...starter }): Example => {
    const prefix = `../../../examples/starter/${directory ?? starter.id}/`;
    const entrypoint = starter.entrypoint ?? 'pipeline.yaml';
    return {
      ...starter,
      category: 'starter',
      requirements: starter.id === 'tic-tac-toe' ? ['model', 'nodeSandbox'] : ['model', 'sandbox'],
      source: starterFiles[prefix + entrypoint],
      files: packageFiles(starterFiles, prefix, entrypoint),
    };
  }),
  ...Object.entries(fixtureTitles).map(([id, [title, description]]) => {
    const prefix = `../../../contracts/v1/fixtures/positive/${id}/`;
    return {
      id,
      titleKey: title,
      descriptionKey: description,
      category: 'block' as const,
      requirements: fixtureRequirements[id],
      kindKey: (
        {
          llm: 'shell.llm',
          agent: 'shell.agent',
          code: 'shell.code',
          tool: 'shell.tool',
          switch: 'shell.switch',
          human: 'shell.human',
          foreach: 'shell.foreach',
          loop: 'shell.loop',
          subpipeline: 'shell.pipeline',
          'artifact-mount': 'shell.artifactMount',
        } as Record<string, MessageKey>
      )[id],
      source: rawFiles[prefix + 'pipeline.yaml'],
      files: packageFiles(rawFiles, prefix),
    };
  }),
];

export function fromExample(example: Example, locale: Locale = 'en'): Workspace {
  let source = example.source;
  // The guided demo relies on its exact canonical source. New starter names
  // become ordinary authoring content once created and are never retranslated.
  if (example.category === 'starter') {
    const document = parseDocument(source);
    document.setIn(['metadata', 'title'], translate(example.titleKey, locale));
    document.setIn(['metadata', 'description'], translate(example.descriptionKey, locale));
    source = document.toString({ lineWidth: 110 });
  }
  return {
    id: crypto.randomUUID(),
    entrypoint: example.entrypoint ?? 'pipeline.yaml',
    source,
    savedSource: source,
    files: example.files.map((file) => ({ ...file })),
    updatedAt: new Date().toISOString(),
  };
}

export function initialWorkspaces(locale: Locale = 'en'): Workspace[] {
  return examples
    .filter((example) => example.category === 'starter')
    .map((example) => fromExample(example, locale));
}
