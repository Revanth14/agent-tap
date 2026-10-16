import type { Alert } from '../types'

// newAlerts returns the daemon's alerts this session has not shown yet. A
// failed or malformed answer yields none: a missed toast beats a wrong one.
export function newAlerts(res: { ok: boolean; text: string } | undefined, shown: readonly string[]): Alert[] {
  if (!res?.ok) return []
  let list: unknown
  try {
    list = JSON.parse(res.text)
  } catch {
    return []
  }
  if (!Array.isArray(list)) return []
  return list.filter(
    (a): a is Alert => typeof a?.id === 'string' && typeof a?.text === 'string' && !shown.includes(a.id),
  )
}
