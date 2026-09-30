import { Validator } from '@cfworker/json-schema';
import spec from '../../../../docs/api/desktop-v1.openapi.json';
import { EngineError } from './error';
const validators = new Map<string, Validator>();
export function assertEngineModel<T>(name: keyof typeof spec.components.schemas, value: unknown): asserts value is T {
  let validator = validators.get(name);
  if (!validator) { validator = new Validator({ $ref: `#/components/schemas/${name}`, components: spec.components }); validators.set(name, validator); }
  if (!validator.validate(value).valid) throw new EngineError('protocol', `Engine returned an invalid ${name} response. The last valid data is preserved.`);
}
