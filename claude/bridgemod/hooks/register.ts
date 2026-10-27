// clicodebridge bridge mod: a structured control channel between the Go
// library and the Claude Code session it runs.
//
// The library loads this plugin with --plugin-dir and passes its endpoint in
// CLICODEBRIDGE_MOD_URL. The mod cannot listen, so it reaches out:
//   POST <url>/mod.hello   once, with the engine version (handshake)
//   POST <url>/mod.poll    long poll; the reply is {"commands":[...]}
//   POST <url>/mod.ack     the outcome of one command
//   POST <url>/mod.event   turn boundaries (turn.complete also reports aborts,
//                          which the Stop settings hook does not)
//   POST <url>/mod.files   files the model sent with the send_file tool
//
// send_file (listed as mcp__clicodebridge__send_file) lets the model hand
// files to the library's client. Claude's own SendUserFile only exists while
// Remote Control is on, and uploads the file to claude.ai; this one stays
// local.
//
// Commands: {id, op: "submit", text} and {id, op: "abort"}.
// The function hooks API is early access; the library falls back to hooks
// and the screen when this mod does not say hello.

const PROTOCOL = 1

type Cmd = { id: string; op: string; text?: string }

let base = ""
let turnId: string | undefined

function post($: any, path: string, body: unknown) {
  return $.http.fetch(`${base}/${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  })
}

function emit($: any, body: Record<string, unknown>) {
  if (base) post($, "mod.event", body).catch(() => {})
}

async function run($: any, cmd: Cmd) {
  try {
    if (cmd.op === "submit") {
      const r = await $.prompt.submit({ text: cmd.text ?? "", asUser: true })
      if (r.drop !== undefined) throw new Error(`dropped: ${r.drop}`)
    } else if (cmd.op === "abort") {
      if (turnId === undefined) throw new Error("no running turn")
      await $.turn.abort({ turnId })
    } else {
      throw new Error(`unknown op ${cmd.op}`)
    }
    await post($, "mod.ack", { id: cmd.id, ok: true })
  } catch (err) {
    await post($, "mod.ack", { id: cmd.id, ok: false, error: String(err) }).catch(() => {})
  }
}

function poll($: any) {
  $.http
    .fetch(`${base}/mod.poll`, { method: "POST", body: "{}" })
    .then((res: any) => {
      const cmds: Cmd[] = res.ok && res.text ? JSON.parse(res.text).commands ?? [] : []
      // Commands run concurrently: a submit resolves only once its turn
      // starts, and an abort must not wait behind it.
      for (const c of cmds) run($, c)
      $.clock.after(res.ok ? 0 : 1000, () => poll($))
    })
    .catch(() => $.clock.after(1000, () => poll($)))
}

const SEND_FILE = {
  name: "send_file",
  description:
    "Send files to the user, who watches this session from another app. Use it for any file the user would want to see " +
    "rather than read about: a generated image or chart, a screenshot, a video, a report, a built artifact. Send " +
    "deliverables as they are produced; do not send scratch files or every intermediate save. Paths may be absolute " +
    "or relative to the working directory, and the files must already exist.",
  inputSchema: {
    type: "object",
    properties: {
      files: { type: "array", items: { type: "string" }, minItems: 1, description: "Paths of the files to send" },
      caption: { type: "string", description: "Optional one-line context" },
    },
    required: ["files"],
  },
}

async function sendFiles($: any, e: any) {
  const cwd = await $.session.cwd()
  const files: { path: string; size: number }[] = []
  for (const f of (e.files ?? []) as string[]) {
    const path = f.startsWith("/") ? f : `${cwd}/${f}`
    let st: any
    try {
      st = await $.fs.stat(path, { resolve: true })
    } catch {
      return { deny: `${f}: no such file` }
    }
    if (st.kind !== "file") return { deny: `${f}: not a regular file` }
    files.push({ path: st.realPath ?? path, size: st.size })
  }
  if (files.length === 0) return { deny: "no files given" }
  const res = await post($, "mod.files", { toolUseId: e.tool_use_id ?? "", caption: e.caption ?? "", files })
  if (!res.ok) return { deny: "the user's client did not accept the files" }
  return { result: `Sent ${files.length} file(s) to the user.` }
}

async function hello($: any) {
  const v = await $.session.version()
  await post($, "mod.hello", { protocol: PROTOCOL, version: v.version, sessionId: await $.session.id() })
}

export const register = (on: any) => {
  on("session.start", async ($: any, e: any, next: any) => {
    const r = await next(e)
    base = (await $.env.get("CLICODEBRIDGE_MOD_URL")) ?? ""
    if (base) {
      await $.tool.register(SEND_FILE)
      await hello($)
      poll($)
    }
    return r
  })

  on("tool.call", { tool: "mcp__clicodebridge__send_file" }, ($: any, e: any) => sendFiles($, e))

  on("turn.start", async ($: any, e: any, next: any) => {
    const r = await next(e)
    turnId = r?.turnId ?? e.turnId
    emit($, { kind: "turn.start", turnId, text: e.text })
    return r
  })

  on("turn.complete", async ($: any, e: any, next: any) => {
    const r = await next(e)
    if (e.agentId === undefined) {
      emit($, { kind: "turn.complete", turnId: e.turnId, reason: e.reason, aborted: e.isAborted, text: r?.text ?? e.answer })
      if (e.turnId === turnId) turnId = undefined
    }
    return r
  })
}
