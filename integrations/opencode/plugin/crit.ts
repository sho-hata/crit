// crit plugin for opencode.
//
// Toasts when the agent starts a blocking `crit` wait so Attention-style
// users notice a review is ready even while the tool call is still running.
//
// Notes captured during implementation:
//   - opencode auto-loads .ts files dropped into `.opencode/plugins/` (project)
//     or `~/.config/opencode/plugins/` (global). No registration in
//     opencode.jsonc is required for local files.

import { createRequire } from "node:module"
import type { Plugin } from "@opencode-ai/plugin"

const require = createRequire(import.meta.url)
const { isCritWaitCommand, roundReadyToast } = require("./lib/crit-wait-notify.js") as {
  isCritWaitCommand: (command: string) => boolean
  roundReadyToast: (url?: string) => { title: string; message: string }
}

function bashCommandFromToolInput(input: any, output: any): string {
  const fromOutput = output?.args?.command ?? output?.args?.cmd
  if (typeof fromOutput === "string") return fromOutput
  const fromInput = input?.args?.command ?? input?.args?.cmd ?? input?.command
  if (typeof fromInput === "string") return fromInput
  return ""
}

function showToast(client: any, title: string, message: string): void {
  try {
    const result = client?.tui?.showToast?.({
      body: { title, message, variant: "info" },
    })
    if (result && typeof result.catch === "function") result.catch(() => {})
  } catch {
    // Toast delivery is best-effort.
  }
  try {
    // SDK expects { body: { service, level, message } } — a flat payload
    // rejects with "Expected object, got undefined" and never reaches the log.
    const result = client?.app?.log?.({
      body: {
        service: "crit",
        level: "info",
        message: `[Crit] ${message}`,
      },
    })
    if (result && typeof result.catch === "function") result.catch(() => {})
  } catch {
    // Logging is best-effort.
  }
}

export const CritNotifyPlugin: Plugin = async ({ client }) => {
  return {
    "tool.execute.before": async (input, output) => {
      const tool = String(input?.tool || "").toLowerCase()
      if (tool !== "bash" && tool !== "shell") return
      const command = bashCommandFromToolInput(input, output)
      if (!isCritWaitCommand(command)) return
      const toast = roundReadyToast()
      showToast(client, toast.title, toast.message)
    },
  }
}
