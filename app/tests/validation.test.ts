import { describe, expect, it } from 'vitest';
import { stringify } from 'yaml';
import { base64, decodeText, textBytes } from '../src/lib/bytes';
import { examples } from '../src/lib/examples';
import { record, type DataSchema, type Pipeline } from '../src/lib/types';
import { parsePipeline, validatePipeline } from '../src/lib/validation';

function example(id: string) {
  const source = examples.find((example) => example.id === id)!;
  return { pipeline: structuredClone(parsePipeline(source.source)!), files: [...source.files] };
}

function errors(pipeline: Pipeline, files = [] as { path: string; content: string }[]) {
  return validatePipeline(
    stringify(pipeline, { aliasDuplicateObjects: false }),
    files,
  ).diagnostics.filter((diagnostic) => diagnostic.severity === 'error');
}

describe('typed package file references', () => {
  it('accepts file properties in JSON defaults and schema annotations as literal data', () => {
    const { pipeline } = example('human');
    const schema = {
      type: 'object',
      properties: { file: { type: 'string' } },
      default: { file: 'schema-default.csv' },
      examples: [{ file: 'schema-example.csv' }],
    };
    pipeline.spec.schemas = { document: schema };
    pipeline.spec.inputs = {
      document: { schemaRef: 'document', default: { file: 'port-default.csv' } },
    };
    expect(errors(pipeline)).toEqual([]);
  });

  for (const [id, field] of [
    ['llm', 'prompt'],
    ['llm', 'instructions'],
    ['agent', 'prompt'],
    ['agent', 'instructions'],
    ['human', 'prompt'],
  ])
    it(`requires declarations and bytes for ${id}.${field}.file`, () => {
      const { pipeline, files } = example(id);
      const node = Object.values(pipeline.spec.nodes).find((node) => node.type === id)!;
      record(node[id])[field] = { file: 'instructions.txt' };
      const supplied = [
        ...files,
        { path: 'instructions.txt', content: base64(textBytes('Read this text.')) },
      ];
      expect(errors(pipeline, supplied).some((error) => error.code === 'FILE_UNDECLARED')).toBe(
        true,
      );
      pipeline.spec.files = [...(pipeline.spec.files ?? []), 'instructions.txt'];
      expect(errors(pipeline, files).some((error) => error.code === 'FILE_MISSING')).toBe(true);
      expect(errors(pipeline, supplied)).toEqual([]);
    });

  it('checks SchemaSource.file rather than objects inside a DataSchema', () => {
    const { pipeline } = example('human');
    pipeline.spec.schemas = { response: { file: 'response.schema.json' } };
    const supplied = [
      { path: 'response.schema.json', content: base64(textBytes('{"type":"string"}')) },
    ];
    expect(errors(pipeline, supplied).some((error) => error.code === 'FILE_UNDECLARED')).toBe(true);
    pipeline.spec.files = ['response.schema.json'];
    expect(errors(pipeline).some((error) => error.code === 'FILE_MISSING')).toBe(true);
    expect(errors(pipeline, supplied)).toEqual([]);
  });

  it('checks text sources in nested graphs using the enclosing Pipeline declarations', () => {
    const { pipeline } = example('foreach');
    const body = record(record(pipeline.spec.nodes.review_all.foreach).body);
    const review = record(record(body.nodes).review);
    record(review.human).prompt = { file: 'nested.txt' };
    pipeline.spec.files = ['nested.txt'];
    expect(errors(pipeline).some((error) => error.code === 'FILE_MISSING')).toBe(true);
    expect(
      errors(pipeline, [{ path: 'nested.txt', content: base64(textBytes('Review the item.')) }]),
    ).toEqual([]);
  });

  it('keeps imported Pipeline file references within their own declarations', () => {
    const { pipeline, files } = example('subpipeline');
    const childFile = files.find((file) => file.path === 'children/writer.yaml')!;
    const child = parsePipeline(decodeText(childFile.content))!;
    const node = Object.values(child.spec.nodes).find((node) => node.type === 'llm')!;
    record(node.llm).prompt = { file: 'child-prompt.txt' };
    pipeline.spec.files = [...(pipeline.spec.files ?? []), 'child-prompt.txt'];
    const supplied = [
      { ...childFile, content: base64(textBytes(stringify(child))) },
      { path: 'child-prompt.txt', content: base64(textBytes('Write a response.')) },
    ];
    expect(errors(pipeline, supplied).some((error) => error.code === 'FILE_UNDECLARED')).toBe(true);
    child.spec.files = ['child-prompt.txt'];
    supplied[0].content = base64(textBytes(stringify(child)));
    expect(errors(pipeline, supplied)).toEqual([]);
    pipeline.spec.files = ['child-prompt.txt'];
    expect(errors(pipeline, supplied).some((error) => error.code === 'FILE_UNDECLARED')).toBe(true);
  });
});

describe('foreach JSON type evidence', () => {
  const schemas: [string, DataSchema][] = [
    ['array union', { type: ['array', 'null'], items: { type: 'string' } }],
    ['boolean schema', true],
    ['local reference', { $defs: { list: { type: 'array' } }, $ref: '#/$defs/list' }],
    ['no explicit type', { items: { type: 'string' } }],
  ];
  for (const [name, schema] of schemas)
    it(`accepts array values permitted by a ${name}`, () => {
      const { pipeline } = example('foreach');
      pipeline.spec.inputs!.topics.schema = schema;
      pipeline.spec.nodes.review_all.inputs!.items.schema = schema;
      expect(errors(pipeline)).toEqual([]);
    });

  it('accepts an array union from the Pipeline schema registry', () => {
    const { pipeline } = example('foreach');
    pipeline.spec.schemas = { items: { type: ['array', 'null'] } };
    const input = pipeline.spec.nodes.review_all.inputs!.items;
    delete input.schema;
    input.schemaRef = 'items';
    expect(errors(pipeline)).toEqual([]);
  });

  for (const schema of [{ type: 'string' }, { type: ['string', 'null'] }])
    it(`rejects a provably non-array input (${JSON.stringify(schema)})`, () => {
      const { pipeline } = example('foreach');
      pipeline.spec.nodes.review_all.inputs!.items.schema = schema;
      expect(errors(pipeline).some((error) => error.code === 'FOREACH_TYPE')).toBe(true);
    });

  it('continues to reject single artifacts as foreach collections', () => {
    const { pipeline } = example('foreach');
    pipeline.spec.nodes.review_all.inputs!.items = {
      artifact: { mediaTypes: ['text/plain'] },
      bind: { from: 'inputs.topics' },
    };
    expect(errors(pipeline).some((error) => error.code === 'FOREACH_TYPE')).toBe(true);
  });
});

describe('schema file format', () => {
  for (const [path, source] of [
    ['response.JSON', '{"type":"string"}'],
    ['response.YAML', 'type: string'],
    ['response.YML', 'type: string'],
  ])
    it(`accepts schema file extensions without changing case (${path})`, () => {
      const { pipeline } = example('human');
      pipeline.spec.schemas = { response: { file: path } };
      pipeline.spec.files = [path];
      expect(errors(pipeline, [{ path, content: base64(textBytes(source)) }])).toEqual([]);
    });

  for (const source of ['type: string', "{'type':'string'}", '{"type":"string",}'])
    it(`rejects YAML syntax in a JSON schema file (${source})`, () => {
      const { pipeline } = example('human');
      pipeline.spec.schemas = { response: { file: 'response.json' } };
      pipeline.spec.files = ['response.json'];
      expect(
        errors(pipeline, [{ path: 'response.json', content: base64(textBytes(source)) }]).some(
          (error) => error.code === 'SCHEMA_INVALID',
        ),
      ).toBe(true);
    });

  it('continues to reject duplicate keys in syntactically valid JSON', () => {
    const { pipeline } = example('human');
    pipeline.spec.schemas = { response: { file: 'response.json' } };
    pipeline.spec.files = ['response.json'];
    expect(
      errors(pipeline, [
        {
          path: 'response.json',
          content: base64(textBytes('{"type":"string","type":"array"}')),
        },
      ]).some((error) => error.layer === 'parse'),
    ).toBe(true);
  });
});
