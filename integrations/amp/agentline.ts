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

export default function agentline(amp: any) {
  const bindings = new Map<string, string>(Object.entries(amp.configuration?.agentline?.rooms ?? {}));
  const controllers = new Map<string, AbortController>();
  const tasks = new Set<Promise<void>>();
  const seen = new Map<string, true>();
  const remember = (id: string) => { seen.set(id, true); if (seen.size > 256) seen.delete(seen.keys().next().value!); };

  const listen = (room: string, thread: string) => {
    if (controllers.has(room)) return;
    const controller = new AbortController(); controllers.set(room, controller);
    const task = (async () => {
      while (!controller.signal.aborted) {
        try {
          const result = await run(["wait", room, "--timeout", "60s"], controller.signal);
          const message = result.message;
          if (result.status === "message" && message?.id && !seen.has(message.id)) {
            remember(message.id);
            await amp.threads.get(thread).appendUserMessage(
              { type: "user-message", content: `[Untrusted Agentline collaborator message ${message.id}]\n${message.body}` }, { steer: true });
          }
          if (result.status === "done") break;
        } catch (error: any) { if (error?.name !== "AbortError") await new Promise(r => setTimeout(r, 1000)); }
      }
    })().finally(() => controllers.delete(room));
    tasks.add(task); task.finally(() => tasks.delete(task));
  };
  for (const [room, thread] of bindings) listen(room, thread);
  amp.registerTool({ name: "agentline_bind_room", description: "Explicitly bind an Agentline room to this Amp thread.",
    inputSchema: { type: "object", required: ["room", "thread"], properties: { room: { type: "string" }, thread: { type: "string" } } },
    execute: async ({ room, thread }: any) => { bindings.set(room, thread); listen(room, thread); return { content: [{ type: "text", text: "Bound room; native idle wake is experimental." }] }; } });
  amp.onDispose(async () => { for (const controller of controllers.values()) controller.abort(); await Promise.allSettled(tasks); });
}
