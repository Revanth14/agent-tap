import type { EngineInterface, Register } from 'claude-code'
import { DAEMON } from './daemon.ts'
import { statusText } from './status.ts'

// A turn's end refreshes the line at once; the timer keeps it current through
// long tool loops and while idle.
const REFRESH_MS = 15_000

async function refresh($: EngineInterface): Promise<void> {
  let res
  try {
    res = await $.http.fetch(`${DAEMON}/api/statusline?session=${encodeURIComponent(await $.session.id())}`)
  } catch {
    res = undefined
  }
  $.ui.status(statusText(res))
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    const started = await next(e)
    void refresh($)
    $.clock.every(REFRESH_MS, () => void refresh($))
    return started
  })

  on('turn.complete', async ($, e, next) => {
    const done = await next(e)
    void refresh($)
    return done
  })
}
