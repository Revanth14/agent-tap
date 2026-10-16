// One of the daemon's GET /api/alerts entries, as the mod reads it.
export type Alert = { id: string; text: string }

declare module 'claude-code' {
  interface PluginState {
    atap: { shownAlerts: string[] }
  }
}
