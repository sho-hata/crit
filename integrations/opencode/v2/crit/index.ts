import { Plugin } from "@opencode/plugin"
import { CritRPC } from "./lib/crit-rpc.js"
import { critWaitTimeout, isCritWaitCommand } from "./lib/crit-wait-notify.js"

export default Plugin.define({
  id: "crit",
  async setup(ctx) {
    const registration = await ctx.rpc.register(CritRPC, {})
    await ctx.shell.hook("create.before", (event) => {
      if (!isCritWaitCommand(event.command)) return
      event.timeout = critWaitTimeout(event.timeout)
    })
    await ctx.tool.hook("execute.before", async (event) => {
      if (event.tool !== "shell") return
      const input = event.input as { command?: unknown }
      if (typeof input.command !== "string" || !isCritWaitCommand(input.command)) return
      await registration.events.emit("waitStarted", {})
    })
  },
})
