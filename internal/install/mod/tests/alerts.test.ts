import { expect, test } from 'claude-code/testing'
import { newAlerts } from '../hooks/alerts.ts'

const answer = (body: unknown) => ({ ok: true, text: JSON.stringify(body) })
const at80 = { id: '5h@1792066800/80', text: '5h plan window at 83% (past 80%) · resets 14:20' }
const at90 = { id: '5h@1792066800/90', text: '5h plan window at 91% (past 90%) · resets 14:20' }

test('returns alerts not shown yet', async () => {
  expect(newAlerts(answer([at80]), [])).toEqual([at80])
  expect(newAlerts(answer([at90]), [at80.id])).toEqual([at90])
})

test('never repeats a shown alert', async () => {
  expect(newAlerts(answer([at80]), [at80.id])).toEqual([])
})

test('ignores a failed or malformed answer', async () => {
  expect(newAlerts(undefined, [])).toEqual([])
  expect(newAlerts({ ok: false, text: '[]' }, [])).toEqual([])
  expect(newAlerts({ ok: true, text: 'not json' }, [])).toEqual([])
  expect(newAlerts(answer({ id: 'x' }), [])).toEqual([])
  expect(newAlerts(answer([{ id: 1, text: 'x' }]), [])).toEqual([])
})
