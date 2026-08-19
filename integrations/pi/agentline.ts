import { spawn } from "node:child_process";

const AGENTLINE = __AGENTLINE_EXECUTABLE_JSON__;

function run(args: string[], signal?: AbortSignal): Promise<any> {
  return new Promise((resolve, reject) => {
    const child = spawn(AGENTLINE, ["--json", ...args], { signal, shell: false });
    let stdout = "";
    child.stdout.on("data", chunk => stdout += chunk);
    child.on("error", reject);
    child.on("close", code => code === 0 ? resolve(JSON.parse(stdout)) : reject(new Error("agentline command failed")));
  });
}

export default function agentline(pi: any) {
  let controller: AbortController | undefined;
  let listener: Promise<void> | undefined;
  const seen = new Map<string, true>();
  const remember = (id: string) => { seen.set(id, true); if (seen.size > 256) seen.delete(seen.keys().next().value!); };

  // The room is bound with `pi --agentline-room ROOM`. Pi's ExtensionContext
  // carries no extension configuration, so a registered CLI flag is the
  // supported way for an extension to read its own settings.
  pi.registerFlag("agentline-room", {
    type: "string",
    description: "Agentline room to deliver collaborator messages from into this session",
  });

  pi.registerTool({ name: "agentline_send_message", parameters: { type: "object", required: ["room", "body", "message_id"] },
    execute: (_id: string, p: any) => run(["send", p.room, p.body, "--message-id", p.message_id]) });
  pi.registerTool({ name: "agentline_wait_for_message", parameters: { type: "object", required: ["room"] },
    execute: (_id: string, p: any, signal: AbortSignal) => run(["wait", p.room, "--timeout", "60s"], signal) });

  pi.on("session_start", (_event: any, ctx: any) => {
    const room = pi.getFlag("agentline-room");
    if (!room || listener) return;
    const isIdle = () => { try { return ctx.isIdle() !== false; } catch { return true; } };
    controller = new AbortController();
    listener = (async () => {
      while (!controller!.signal.aborted) {
        try {
          const result = await run(["wait", room, "--timeout", "60s"], controller!.signal);
          const message = result.message;
          if (result.status === "message" && message?.id && !seen.has(message.id)) {
            remember(message.id);
            // Pi starts a turn when idle; while it is mid-turn the message has
            // to be queued instead, or it lands in a busy session. ctx outlives
            // the event that delivered it, so treat an unusable one as idle:
            // the worst case is a message Pi has to queue itself, whereas
            // wrongly assuming busy would suppress the wake entirely.
            pi.sendUserMessage(`[Untrusted Agentline collaborator message ${message.id}]\n${message.body}`,
              isIdle() ? undefined : { deliverAs: "followUp" });
          }
          if (result.status === "done") break;
        } catch (error: any) { if (error?.name !== "AbortError") await new Promise(r => setTimeout(r, 1000)); }
      }
    })();
  });
  pi.on("session_shutdown", async () => { controller?.abort(); await listener; listener = undefined; controller = undefined; });
}
