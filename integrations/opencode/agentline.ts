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

export const AgentlinePlugin = async ({ client }: any) => {
  const controllers = new Map<string, AbortController>();
  const seen = new Map<string, true>();
  const running = new Set<string>();
  const remember = (id: string) => { seen.set(id, true); if (seen.size > 256) seen.delete(seen.keys().next().value!); };
  const start = (session: string, room: string, experimentalPrompt: boolean) => {
    if (controllers.has(session)) return;
    const controller = new AbortController(); controllers.set(session, controller);
    void (async () => {
      while (!controller.signal.aborted) {
        try {
          const result = await run(["wait", room, "--timeout", "60s"], controller.signal);
          const message = result.message;
          if (result.status === "message" && message?.id && !seen.has(message.id)) {
            remember(message.id);
            const text = `[Untrusted Agentline collaborator message ${message.id}]\n${message.body}`;
            await client.session.message({ path: { id: session }, body: { role: "user", parts: [{ type: "text", text }] } });
            if (experimentalPrompt && !running.has(session)) {
              running.add(session);
              try { await client.session.prompt({ path: { id: session }, body: { parts: [{ type: "text", text }] } }); }
              finally { running.delete(session); }
            }
          }
          if (result.status === "done") break;
        } catch (error: any) { if (error?.name !== "AbortError") await new Promise(r => setTimeout(r, 1000)); }
      }
    })().finally(() => controllers.delete(session));
  };
  return { event: async ({ event }: any) => {
    if (event.type === "session.created") {
      const config = event.properties?.info?.agentline;
      if (config?.room) start(event.properties.info.id, config.room, config.experimentalAutomaticPrompt === true);
    }
    if (event.type === "session.deleted") controllers.get(event.properties.info.id)?.abort();
  } };
};
