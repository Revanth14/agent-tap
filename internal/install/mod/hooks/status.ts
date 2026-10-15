// statusText is the daemon's status line, or "atap: offline" when it did not
// answer. The daemon composes the line; the mod only shows it.
export function statusText(res: { ok: boolean; text: string } | undefined): string {
  return res?.ok ? res.text.trim() : 'atap: offline'
}
