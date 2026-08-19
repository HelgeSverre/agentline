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

export default function agentline(amp: any) {
  const controllers = new Map<string, AbortController>();
  const bindings = new Map<string, string>();
  const tasks = new Set<Promise<void>>();
  const seen = new Map<string, true>();
  const remember = (id: string) => { seen.set(id, true); if (seen.size > 256) seen.delete(seen.keys().next().value!); };

  // threadID comes from the thread the bind tool ran in, so routing is explicit
  // and never depends on whichever thread happens to be focused when a peer
  // writes. The thread is resolved fresh on each delivery rather than held as a
  // captured object, which may go stale over the life of a long listener.
  // Rebinding a room to a different thread replaces the previous listener.
  const listen = (room: string, threadID: string) => {
    if (bindings.get(room) === threadID) return false;
    controllers.get(room)?.abort();
    const controller = new AbortController();
    controllers.set(room, controller); bindings.set(room, threadID);
    const task = (async () => {
      while (!controller.signal.aborted) {
        try {
          const result = await run(["wait", room, "--timeout", "60s"], controller.signal);
          const message = result.message;
          if (result.status === "message" && message?.id && !seen.has(message.id)) {
            remember(message.id);
            await amp.threads.get(threadID).appendUserMessage(
              { type: "user-message", content: `[Untrusted Agentline collaborator message ${message.id}]\n${message.body}` },
              { steer: true });
          }
          if (result.status === "done") break;
        } catch (error: any) { if (error?.name !== "AbortError") await new Promise(r => setTimeout(r, 1000)); }
      }
      // Only clear the entry if this listener is still the current one; a
      // rebind may already have installed its replacement.
    })().finally(() => { if (controllers.get(room) === controller) controllers.delete(room); });
    tasks.add(task); task.finally(() => tasks.delete(task));
    return true;
  };

  // A room is bound by calling this tool from the thread that should receive
  // the messages. amp.configuration is an async store rather than a plain
  // object, and the tool context already carries the thread, so the binding
  // never has to be guessed or configured ahead of time.
  amp.registerTool({
    name: "agentline_bind_room",
    description: "Bind an Agentline room to this Amp thread so collaborator messages are appended to it. Native idle wake is experimental.",
    inputSchema: { type: "object", required: ["room"], properties: { room: { type: "string", description: "Room ID or saved room name" } } },
    execute: async ({ room }: any, ctx: any) => {
      const started = listen(room, ctx.thread.id);
      return { content: [{ type: "text", text: started
        ? `Bound room ${room} to thread ${ctx.thread.id}; native idle wake is experimental.`
        : `Room ${room} is already bound to thread ${ctx.thread.id}.` }] };
    },
  });

  amp.onDispose(async () => { for (const controller of controllers.values()) controller.abort(); await Promise.allSettled(tasks); });
}
