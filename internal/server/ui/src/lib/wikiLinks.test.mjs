import { test } from 'node:test'
import assert from 'node:assert/strict'

import { taskWikiMarkdown } from './wikiLinks.ts'

test('taskWikiMarkdown links known task slugs and leaves unresolved tokens inert', () => {
  assert.equal(
    taskWikiMarkdown('See [[target-task]] and [[missing-task]] and [[target-task|Alias]].', new Set(['target-task'])),
    'See [target-task](#task:target-task) and [[missing-task]] and [[target-task|Alias]].',
  )
})

test('taskWikiMarkdown trims link text without changing surrounding markdown', () => {
  assert.equal(
    taskWikiMarkdown('Before **[[ target-task ]]** after', new Set(['target-task'])),
    'Before **[target-task](#task:target-task)** after',
  )
})
