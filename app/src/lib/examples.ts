import { stringify } from 'yaml';
import localSource from '../../../examples/local/pipeline.yaml?raw';
import { base64, textBytes } from './bytes';
import type { PackageFile, Pipeline, Workspace } from './types';

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
  title: string;
  description: string;
  kind: string;
  source: string;
  files: PackageFile[];
}

const rawFiles = import.meta.glob('../../../contracts/v1/fixtures/positive/**/*', {
  eager: true,
  query: '?raw',
  import: 'default',
}) as Record<string, string>;
const fixtureTitles: Record<string, [string, string]> = {
  llm: ['Model response', 'Structured generation with a prompt and an external schema.'],
  agent: ['Autonomous research', 'A bounded agent with MCP tools and an isolated sandbox.'],
  code: ['Create an artifact', 'Run a command and collect a declared output file.'],
  tool: ['MCP tool call', 'Call a specific tool with a structured response.'],
  switch: ['Conditional routing', 'Select a branch and coalesce the resulting outputs.'],
  human: ['Human review', 'Wait for a response that matches the output contract.'],
  foreach: ['Parallel review', 'Review a collection with isolated, concurrent iterations.'],
  loop: ['Iterative refinement', 'Carry state through a bounded loop.'],
  subpipeline: ['Reusable pipeline', 'Invoke a child package with explicit permissions.'],
  'artifact-mount': ['Pass a file', 'Mount a declared artifact in a separate sandbox.'],
};
export const examples: Example[] = [
  {
    id: 'research',
    title: 'Research brief',
    description: 'Explore the complete workbench with a guided demo.',
    kind: '5 nodes · demo available',
    source: RESEARCH_SOURCE,
    files: [],
  },
  {
    id: 'local',
    title: 'Local Ollama greeting',
    description: 'Generate and save a greeting with the bundled local engine profile.',
    kind: 'llm + code · local profile',
    source: localSource,
    files: [],
  },
  ...Object.entries(fixtureTitles).map(([id, [title, description]]) => {
    const prefix = `../../../contracts/v1/fixtures/positive/${id}/`;
    return {
      id,
      title,
      description,
      kind: id === 'subpipeline' ? 'pipeline' : id,
      source: rawFiles[prefix + 'pipeline.yaml'],
      files: Object.entries(rawFiles)
        .filter(([path]) => path.startsWith(prefix) && path !== prefix + 'pipeline.yaml')
        .map(([path, content]) => ({
          path: path.slice(prefix.length),
          content: base64(textBytes(content)),
        })),
    };
  }),
];

export function fromExample(example: Example): Workspace {
  return {
    id: crypto.randomUUID(),
    entrypoint: 'pipeline.yaml',
    source: example.source,
    savedSource: example.source,
    files: example.files,
    updatedAt: new Date().toISOString(),
  };
}

export function initialWorkspaces(): Workspace[] {
  return [
    examples[0],
    examples.find((e) => e.id === 'agent')!,
    examples.find((e) => e.id === 'foreach')!,
  ].map(fromExample);
}
