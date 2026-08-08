import { spawn } from "node:child_process";

const AGENTLINE = __AGENTLINE_EXECUTABLE_JSON__;
const WAIT_MS = 60000;

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

  pi.registerTool({ name: "agentline_send_message", parameters: { type: "object", required: ["room", "body", "message_id"] },
    execute: (_id: string, p: any) => run(["send", p.room, p.body, "--message-id", p.message_id]) });
  pi.registerTool({ name: "agentline_wait_for_message", parameters: { type: "object", required: ["room"] },
    execute: (_id: string, p: any, signal: AbortSignal) => run(["wait", p.room, "--timeout", "60s"], signal) });

  pi.on("session_start", (_event: any, ctx: any) => {
    const room = ctx?.config?.agentline?.room;
    if (!room || listener) return;
    controller = new AbortController();
    listener = (async () => {
      while (!controller!.signal.aborted) {
        try {
          const result = await run(["wait", room, "--timeout", "60s"], controller!.signal);
          const message = result.message;
          if (result.status === "message" && message?.id && !seen.has(message.id)) {
            remember(message.id);
            pi.sendUserMessage({ type: "text", text: `[Untrusted Agentline collaborator message ${message.id}]\n${message.body}` },
              pi.isStreaming ? { deliverAs: "followUp" } : undefined);
          }
          if (result.status === "done") break;
        } catch (error: any) { if (error?.name !== "AbortError") await new Promise(r => setTimeout(r, 1000)); }
      }
    })();
  });
  pi.on("session_shutdown", async () => { controller?.abort(); await listener; listener = undefined; controller = undefined; });
}
