import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'
import { newAlerts } from './alerts.ts'
import { DAEMON } from './daemon.ts'
import { statusText } from './status.ts'

// A turn's end refreshes the line at once; the timer keeps it current through
// long tool loops and while idle.
const REFRESH_MS = 15_000
const TOAST_MS = 15_000 // How long an alert stays

// The alerts this session has shown. Session state outlives a reload of the
// mod, so a reload does not repeat them.
const shown = atom({ plugin: 'atap', key: 'shownAlerts' } as const, [])

let checking = false // A timer tick and a turn's end must not toast the same alert twice

async function fetchDaemon($: EngineInterface, path: string) {
  try {
    return await $.http.fetch(`${DAEMON}${path}`)
  } catch {
    return undefined
  }
}

async function refresh($: EngineInterface): Promise<void> {
  const session = encodeURIComponent(await $.session.id())
  $.ui.status(statusText(await fetchDaemon($, `/api/statusline?session=${session}`)))

  if (checking) return
  checking = true
  try {
    for (const alert of newAlerts(await fetchDaemon($, '/api/alerts'), await read($, shown))) {
      $.ui.toast(alert.text, { timeoutMs: TOAST_MS })
      await update($, shown, ids => [...ids, alert.id].slice(-50))
    }
  } finally {
    checking = false
  }
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
