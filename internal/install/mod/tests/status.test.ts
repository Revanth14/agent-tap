import { expect, test } from 'claude-code/testing'
import { statusText } from '../hooks/status.ts'

test("shows the daemon's line", async () => {
  expect(statusText({ ok: true, text: '5h 91% · +6%/h · full ~1h10m\n' })).toBe('5h 91% · +6%/h · full ~1h10m')
})

test('says offline when the daemon does not answer', async () => {
  expect(statusText(undefined)).toBe('atap: offline')
  expect(statusText({ ok: false, text: 'Forbidden' })).toBe('atap: offline')
})
