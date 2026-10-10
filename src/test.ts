/// <reference types="vite/client" />
import type { TestConvex } from 'convex-test'
import { componentsGeneric, type GenericSchema, type SchemaDefinition } from 'convex/server'
import schema from './component/schema.js'
import type { ComponentApi } from './component/_generated/component.js'

const modules = import.meta.glob([
  './component/**/*.ts',
  '!./component/**/*.test.ts',
  '!./component/test.modules.ts',
])

/**
 * Registers the component with a convex-test instance.
 * @param t - The test instance, e.g. from calling `convexTest`.
 * @param name - The name of the component, as registered in convex.config.ts.
 */
export function register<Schema extends SchemaDefinition<GenericSchema, boolean>>(
  t: TestConvex<Schema>,
  name: string = 'mailBridge',
) {
  t.registerComponent(name, schema, modules)
  return componentsGeneric()[name] as unknown as ComponentApi
}

export default { register, schema, modules }
