import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const tasksSource = readFileSync(resolve(here, 'Tasks.tsx'), 'utf8')

test('tasks table renders a named runtime chip from live runtime state', () => {
  assert.match(tasksSource, /function taskRuntimeStatus\(task: TaskView\)/)
  assert.match(tasksSource, /task\.runtime_status/)
  assert.match(tasksSource, /task\.live/)
  assert.match(tasksSource, /function RuntimeChip/)
  assert.match(tasksSource, /className=\{`runtime-chip/)
  assert.match(tasksSource, /<RuntimeChip task=\{task\} \/>/)
})
