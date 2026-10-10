/// <reference types="vite/client" />
import { test } from 'vitest'
import { convexTest } from 'convex-test'
import schema from './schema.js'
import component from '@nordwerk/convex-mail-bridge/test'

const modules = import.meta.glob('./**/*.*s')

// Tests of an app that uses the component register it with its schema and modules.
export function initConvexTest() {
  const t = convexTest(schema, modules)
  component.register(t)
  return t
}

test('setup', () => {})
