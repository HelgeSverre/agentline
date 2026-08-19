import { spawn } from "node:child_process";
import { tool } from "@opencode-ai/plugin";

const AGENTLINE = __AGENTLINE_EXECUTABLE_JSON__;

// Automatic prompting can start a billed turn without a human present, so it is
// opt-in through the environment rather than through a tool argument the model
// could set for itself.
const EXPERIMENTAL_PROMPT = process.env.AGENTLINE_OPENCODE_EXPERIMENTAL_PROMPT === "1";

function run(args: string[], signal?: AbortSignal): Promise<any> {
  return new Promise((resolve, reject) => {
    const child = spawn(AGENTLINE, ["--json", ...args], { signal, shell: false });
    let stdout = "";
    child.stdout.on("data", chunk => stdout += chunk);
    child.on("error", reject);
    child.on("close", code => code === 0 ? resolve(JSON.parse(stdout)) : reject(new Error("agentline command failed")));
  });
}

export const AgentlinePlugin = async ({ client }: any) => {
  const controllers = new Map<string, AbortController>();
  const seen = new Map<string, true>();
  const running = new Set<string>();
  const bound = new Map<string, string>();
  const remember = (id: string) => { seen.set(id, true); if (seen.size > 256) seen.delete(seen.keys().next().value!); };

  // Rebinding a session to a different room replaces the previous listener
  // rather than silently keeping the old one.
  const start = (session: string, room: string) => {
    if (bound.get(session) === room) return false;
    controllers.get(session)?.abort();
    const controller = new AbortController();
    controllers.set(session, controller); bound.set(session, room);
    void (async () => {
      while (!controller.signal.aborted) {
        try {
          const result = await run(["wait", room, "--timeout", "60s"], controller.signal);
          const message = result.message;
          if (result.status === "message" && message?.id && !seen.has(message.id)) {
            remember(message.id);
            const text = `[Untrusted Agentline collaborator message ${message.id}]\n${message.body}`;
            await client.session.message({ path: { id: session }, body: { role: "user", parts: [{ type: "text", text }] } });
            if (EXPERIMENTAL_PROMPT && !running.has(session)) {
              running.add(session);
              try { await client.session.prompt({ path: { id: session }, body: { parts: [{ type: "text", text }] } }); }
              finally { running.delete(session); }
            }
          }
          if (result.status === "done") break;
        } catch (error: any) { if (error?.name !== "AbortError") await new Promise(r => setTimeout(r, 1000)); }
      }
      // Only clear the entry if this listener is still the current one; a
      // rebind may already have installed its replacement.
    })().finally(() => { if (controllers.get(session) === controller) controllers.delete(session); });
    return true;
  };

  return {
    // A room is bound explicitly through this tool. OpenCode's Session object
    // carries no user fields, and locally installed plugins receive no options,
    // so the tool context's sessionID is the only way to learn which session a
    // room belongs to.
    tool: {
      agentline_bind_room: tool({
        description: "Bind an Agentline room to this OpenCode session so collaborator messages are delivered into it. Native idle wake is experimental; the bounded wait_for_message loop remains the guaranteed path.",
        args: { room: tool.schema.string().describe("Room ID or saved room name") },
        async execute(args, context) {
          const started = start(context.sessionID, args.room);
          return started
            ? `Bound room ${args.room} to this session; native idle wake is experimental.`
            : `This session is already bound to room ${args.room}.`;
        },
      }),
    },
    event: async ({ event }: any) => {
      if (event.type === "session.deleted") {
        const session = event.properties.info.id;
        controllers.get(session)?.abort(); bound.delete(session);
      }
    },
  };
};
