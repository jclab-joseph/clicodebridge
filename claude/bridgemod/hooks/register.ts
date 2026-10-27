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

async function hello($: any) {
  const v = await $.session.version()
  await post($, "mod.hello", { protocol: PROTOCOL, version: v.version, sessionId: await $.session.id() })
}

export const register = (on: any) => {
  on("session.start", async ($: any, e: any, next: any) => {
    const r = await next(e)
    base = (await $.env.get("CLICODEBRIDGE_MOD_URL")) ?? ""
    if (base) {
      await hello($)
      poll($)
    }
    return r
  })

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
