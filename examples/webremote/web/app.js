"use strict";

const $ = (sel) => document.querySelector(sel);
const el = (tag, attrs = {}, ...kids) => {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") n.className = v;
    else if (k === "text") n.textContent = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) n.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) if (kid != null) n.append(kid);
  return n;
};

const store = {
  get(k, d) { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch {} },
};

async function api(path, { method = "GET", body } = {}) {
  const res = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
  return res.status === 204 || res.status === 202 ? null : res.json();
}

let toastTimer;
function toast(msg) {
  const t = $("#toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), 4500);
}

const STATE_LABEL = { starting: "Starting", idle: "Ready", busy: "Working", waiting: "Needs you", exited: "Exited" };

// ---- app state ----------------------------------------------------------------

const app = {
  providers: [],
  sessions: [],
  current: null, // { key, es, summary, tools: Map, cards: Map, term, termEs }
};

function providerInfo(name) {
  return app.providers.find((p) => p.name === name) || { models: [], efforts: [], modes: [] };
}

// ---- sidebar ---------------------------------------------------------------------

async function refreshSessions() {
  try {
    app.sessions = await api("api/sessions");
  } catch {
    return;
  }
  const list = $("#session-list");
  list.replaceChildren(
    ...app.sessions.map((s) =>
      el("button", {
        class: "session-item" + (app.current?.key === s.key ? " active" : ""),
        onclick: () => openSession(s.key),
      },
        el("span", { class: "row" },
          el("span", { class: "provider-tag", text: s.provider }),
          el("span", { class: `state ${s.state}`, text: STATE_LABEL[s.state] || s.state }),
          s.pending?.length ? el("span", { class: "badge", text: String(s.pending.length) }) : null),
        el("span", { class: "path", text: s.cwd, title: s.cwd }))
    )
  );
  $("#empty-hint").hidden = app.sessions.length > 0;
}

// ---- new session dialog -------------------------------------------------------------

function fillChoices(select, choices, { includeDefault = true, value = "" } = {}) {
  select.replaceChildren(
    ...(includeDefault ? [el("option", { value: "", text: "Default" })] : []),
    ...choices.filter((c) => c.id !== "").map((c) => el("option", { value: c.id, text: c.name || c.id, title: c.description || "" }))
  );
  select.value = value;
}

function fillModels(datalist, models) {
  datalist.replaceChildren(...models.filter((m) => m.id).map((m) => el("option", { value: m.id, label: m.name })));
}

function syncProviderFields() {
  const name = $("#f-provider").value;
  const info = providerInfo(name);
  fillModels($("#f-model-list"), info.models);
  fillChoices($("#f-effort"), info.efforts);
  fillChoices($("#f-mode"), info.modes);
  document.querySelectorAll(".claude-only").forEach((n) => (n.hidden = name !== "claude"));
  document.querySelectorAll(".codex-only").forEach((n) => (n.hidden = name !== "codex"));
}

async function openNewDialog() {
  await app.providersReady;
  const sel = $("#f-provider");
  sel.replaceChildren(...app.providers.map((p) => el("option", { value: p.name, text: p.displayName + (p.version ? ` (${p.version})` : "") })));
  sel.value = store.get("provider", app.providers[0]?.name || "claude");
  $("#f-cwd").value = store.get("cwd", "");
  syncProviderFields();
  $("#new-dialog").showModal();
}

async function submitNew(ev) {
  if (ev.submitter?.value !== "ok") return;
  ev.preventDefault();
  const form = new FormData($("#new-form"));
  const body = Object.fromEntries(form.entries());
  body.trustWorkspace = form.has("trustWorkspace");
  body.search = form.has("search");
  store.set("provider", body.provider);
  store.set("cwd", body.cwd);
  const btn = $("#f-start");
  btn.disabled = true;
  btn.textContent = "Starting…";
  try {
    const s = await api("api/sessions", { method: "POST", body });
    $("#new-dialog").close();
    await refreshSessions();
    openSession(s.key);
  } catch (e) {
    toast(e.message);
  } finally {
    btn.disabled = false;
    btn.textContent = "Start";
  }
}

// ---- session view ----------------------------------------------------------------------

function closeCurrent() {
  const c = app.current;
  if (!c) return;
  c.es?.close();
  c.termEs?.close();
  c.term?.dispose();
  app.current = null;
}

function openSession(key) {
  closeCurrent();
  app.current = { key, tools: new Map(), cards: new Map(), summary: null, replaying: true };
  $("#placeholder").hidden = true;
  $("#session").hidden = false;
  document.querySelector(".app").classList.add("in-session");
  $("#log").replaceChildren();
  $("#requests").replaceChildren();
  showTab("chat");
  const es = new EventSource(`api/sessions/${key}/events`);
  app.current.es = es;
  es.addEventListener("summary", (m) => applySummary(JSON.parse(m.data)));
  es.addEventListener("event", (m) => handleEvent(JSON.parse(m.data)));
  es.addEventListener("replayed", () => {
    app.current.replaying = false;
    scrollLog(true);
  });
  refreshSessions();
}

function applySummary(s) {
  const c = app.current;
  if (!c) return;
  if (c.summary?.state !== s.state || (c.summary?.pending?.length || 0) !== (s.pending?.length || 0)) refreshSessions();
  c.summary = s;
  const info = providerInfo(s.provider);
  $("#s-provider").textContent = info.displayName || s.provider;
  const st = $("#s-state");
  st.className = `state ${s.state}`;
  st.textContent = STATE_LABEL[s.state] || s.state;
  $("#s-cwd").textContent = s.cwd + (s.id ? `  ·  ${s.id}` : "");
  $("#s-cwd").title = s.cwd;
  fillModels($("#model-list"), info.models);
  const model = $("#s-model");
  if (document.activeElement !== model) model.value = s.settings.model || "";
  fillChoices($("#s-effort"), info.efforts, { value: s.settings.effort || "" });
  fillChoices($("#s-mode"), info.modes, { value: s.settings.mode || "" });
  const rc = $("#s-rc");
  rc.hidden = !s.settings.remoteControlUrl;
  rc.href = s.settings.remoteControlUrl || "#";
  const exited = s.state === "exited";
  for (const id of ["#send", "#s-interrupt", "#s-close", "#s-model", "#s-effort", "#s-mode"]) $(id).disabled = exited;
}

function scrollLog(force) {
  const log = $("#log");
  if (force || log.scrollHeight - log.scrollTop - log.clientHeight < 160) log.scrollTop = log.scrollHeight;
}

function append(node) {
  $("#log").append(node);
  if (!app.current?.replaying) scrollLog(false);
}

// A small, safe markdown subset: fenced code, inline code, bold, paragraphs.
function renderMarkdown(text) {
  const frag = document.createDocumentFragment();
  const parts = text.split(/```[^\n]*\n?/);
  parts.forEach((part, i) => {
    if (i % 2 === 1) {
      frag.append(el("pre", { text: part.replace(/\n$/, "") }));
      return;
    }
    for (const para of part.split(/\n{2,}/)) {
      if (!para.trim()) continue;
      const p = el("p");
      for (const tok of para.split(/(`[^`]+`|\*\*[^*]+\*\*)/)) {
        if (tok.startsWith("`") && tok.endsWith("`") && tok.length > 2) p.append(el("code", { text: tok.slice(1, -1) }));
        else if (tok.startsWith("**") && tok.endsWith("**") && tok.length > 4) p.append(el("strong", { text: tok.slice(2, -2) }));
        else p.append(tok);
      }
      frag.append(p);
    }
  });
  return frag;
}

function toolSummary(tool) {
  const v = tool.input;
  if (!v) return "";
  if (typeof v === "string") return v.split("\n")[0];
  for (const k of ["command", "file_path", "url", "pattern", "path", "description"]) if (v[k]) return String(v[k]);
  return JSON.stringify(v);
}

function handleEvent(ev) {
  const c = app.current;
  if (!c) return;
  switch (ev.type) {
    case "user_message":
      append(el("div", { class: "msg user" + (ev.kind === "command" ? " command" : ""), text: ev.text }));
      break;
    case "assistant_text":
      append(el("div", { class: "msg assistant" }, renderMarkdown(ev.text)));
      break;
    case "thinking":
      append(el("details", { class: "thinking" }, el("summary", { text: "Thinking" }), el("div", { text: ev.text })));
      break;
    case "tool_call": {
      const t = ev.tool || {};
      const body = el("div", {}, el("pre", { text: t.input ? JSON.stringify(t.input, null, 2) : "" }));
      const node = el("details", { class: "tool" },
        el("summary", {}, el("b", { text: t.name || "tool" }), el("span", { text: toolSummary(t) })), body);
      if (t.id) c.tools.set(t.id, node);
      append(node);
      break;
    }
    case "tool_result": {
      const t = ev.tool || {};
      const node = c.tools.get(t.id);
      const out = el("pre", { class: "result", text: t.output || "(no output)" });
      if (node) {
        node.lastChild.append(out);
        if (t.isError) node.classList.add("error");
      } else {
        append(el("details", { class: "tool" + (t.isError ? " error" : "") }, el("summary", {}, el("b", { text: "result" })), el("div", {}, out)));
      }
      break;
    }
    case "turn_completed":
      append(el("div", { class: "turn-end", text: ev.interrupted ? "Turn interrupted" : `Turn finished · ${new Date(ev.time).toLocaleTimeString()}` }));
      break;
    case "request":
      showRequest(ev.request);
      break;
    case "request_resolved": {
      const card = c.cards.get(ev.requestId);
      if (card) {
        card.remove();
        c.cards.delete(ev.requestId);
      }
      const how = { client: "answered here", external: "answered in the CLI or Remote Control", cancelled: "withdrawn" }[ev.resolution] || ev.resolution;
      append(el("div", { class: "notice", text: `Request ${how}` }));
      break;
    }
    case "notice":
      if (ev.kind !== "idle_prompt") append(el("div", { class: "notice", text: ev.text }));
      break;
    case "restarted":
      append(el("div", { class: "notice", text: "CLI relaunched with the new settings" }));
      if (c.term) connectTerminal();
      break;
    case "error":
      append(el("div", { class: "notice error", text: ev.text }));
      break;
    case "exited":
      append(el("div", { class: "notice error", text: `Session ended: ${ev.text}` }));
      refreshSessions();
      break;
  }
}

// ---- requests ---------------------------------------------------------------------

async function respond(req, response, card) {
  card.querySelectorAll("button, input").forEach((b) => (b.disabled = true));
  try {
    await api(`api/sessions/${app.current.key}/respond`, { method: "POST", body: { requestId: req.id, response } });
  } catch (e) {
    toast(e.message);
    card.querySelectorAll("button, input").forEach((b) => (b.disabled = false));
  }
}

function showRequest(req) {
  const c = app.current;
  if (c.cards.has(req.id)) return;
  const card = req.kind === "question" ? questionCard(req) : permissionCard(req);
  c.cards.set(req.id, card);
  $("#requests").append(card);
  if (!c.replaying) card.scrollIntoView({ block: "nearest" });
}

function permissionCard(req) {
  const p = req.permission || { options: [] };
  const card = el("div", { class: "req" });
  const note = el("input", { class: "other", placeholder: "Optional note for Deny (what to do instead)" });
  card.append(
    el("h3", { text: req.title }),
    p.detail ? el("pre", { text: p.detail }) : null,
    el("div", { class: "opts" },
      ...p.options.map((o) =>
        el("button", {
          class: o.kind === "allow_once" ? "primary" : o.kind === "deny" ? "danger" : "",
          text: o.label,
          onclick: () => respond(req, { optionId: o.id, message: o.kind === "deny" ? note.value : "" }, card),
        }))),
    p.options.some((o) => o.kind === "deny") ? note : null
  );
  return card;
}

function questionCard(req) {
  const card = el("div", { class: "req" });
  const readers = req.questions.map((q, qi) => {
    const name = `${req.id}-${qi}`;
    const type = q.multiSelect ? "checkbox" : "radio";
    const inputs = q.options.map((o) => el("input", { type, name, value: o.label }));
    const other = q.allowText ? el("input", { class: "other", placeholder: q.multiSelect ? "Add your own answer (optional)" : "Or type your own answer" }) : null;
    if (other && !q.multiSelect) {
      other.addEventListener("input", () => other.value && inputs.forEach((i) => (i.checked = false)));
      inputs.forEach((i) => i.addEventListener("change", () => (other.value = "")));
    }
    card.append(
      el("div", { class: "q" },
        el("div", { class: "q-head" }, q.header ? el("span", { class: "chip", text: q.header }) : null, el("strong", { text: q.text })),
        ...q.options.map((o, oi) => el("label", { class: "choice" }, inputs[oi], el("span", {}, o.label, o.description ? el("small", { text: o.description }) : null))),
        other)
    );
    return () => ({
      selected: inputs.filter((i) => i.checked).map((i) => i.value),
      text: other?.value.trim() || "",
    });
  });
  card.prepend(el("h3", { text: req.questions.length > 1 ? "Questions" : "Question" }));
  card.append(
    el("div", { class: "row-end" },
      el("button", { text: "Dismiss", onclick: () => respond(req, { cancel: true }, card) }),
      el("button", {
        class: "primary",
        text: "Submit",
        onclick: () => {
          const answers = readers.map((r) => r());
          if (answers.some((a) => !a.selected.length && !a.text)) return toast("Answer every question first");
          respond(req, { answers }, card);
        },
      }))
  );
  return card;
}

// ---- composer and controls ---------------------------------------------------------------

async function sendPrompt(ev) {
  ev?.preventDefault();
  const ta = $("#prompt");
  const text = ta.value.trim();
  if (!text || !app.current) return;
  const isCommand = /^\/\S/.test(text) && !text.includes("\n");
  $("#send").disabled = true;
  ta.value = "";
  try {
    if (isCommand) await api(`api/sessions/${app.current.key}/command`, { method: "POST", body: { command: text } });
    else await api(`api/sessions/${app.current.key}/send`, { method: "POST", body: { text } });
  } catch (e) {
    if (!ta.value) ta.value = text;
    toast(e.message);
  } finally {
    $("#send").disabled = false;
    ta.focus();
  }
}

async function changeSetting(field, value) {
  if (!app.current) return;
  toast(`Applying ${field}…`);
  try {
    await api(`api/sessions/${app.current.key}/settings`, { method: "POST", body: { [field]: value } });
    toast(`${field} set to ${value || "default"}`);
  } catch (e) {
    toast(e.message);
    if (app.current.summary) applySummary(app.current.summary);
  }
}

// ---- terminal ------------------------------------------------------------------------------

function showTab(name) {
  document.querySelectorAll(".tab").forEach((t) => {
    const on = t.dataset.tab === name;
    t.classList.toggle("active", on);
    t.setAttribute("aria-selected", on);
  });
  $("#panel-chat").hidden = name !== "chat";
  $("#panel-terminal").hidden = name !== "terminal";
  if (name === "terminal" && app.current && !app.current.term) connectTerminal();
  if (name === "terminal") app.current?.term?.focus();
}

function decode(b64) {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function connectTerminal() {
  const c = app.current;
  if (!c || typeof Terminal === "undefined") return;
  c.termEs?.close();
  if (!c.term) {
    c.term = new Terminal({ fontFamily: "ui-monospace, Menlo, Consolas, monospace", fontSize: 13, cursorBlink: true, scrollback: 0 });
    $("#term").replaceChildren();
    c.term.open($("#term"));
    let chain = Promise.resolve();
    c.term.onData((data) => {
      const key = c.key;
      chain = chain.then(() => api(`api/sessions/${key}/terminal`, { method: "POST", body: { data } }).catch((e) => toast(e.message)));
    });
  }
  const es = new EventSource(`api/sessions/${c.key}/terminal`);
  c.termEs = es;
  es.addEventListener("reset", (m) => {
    const r = JSON.parse(m.data);
    c.term.reset();
    c.term.resize(r.cols, r.rows);
    c.term.write(decode(r.data));
  });
  es.addEventListener("data", (m) => c.term.write(decode(m.data)));
  es.addEventListener("exit", () => es.close());
}

// ---- wiring -------------------------------------------------------------------------------

async function init() {
  app.providersReady = api("api/providers")
    .then((p) => (app.providers = p || []))
    .catch((e) => toast(e.message));
  $("#new-session").addEventListener("click", openNewDialog);
  $("#f-provider").addEventListener("change", syncProviderFields);
  $("#new-form").addEventListener("submit", submitNew);
  $("#composer").addEventListener("submit", sendPrompt);
  $("#prompt").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) sendPrompt(e);
  });
  $("#s-interrupt").addEventListener("click", () =>
    app.current && api(`api/sessions/${app.current.key}/interrupt`, { method: "POST" }).catch((e) => toast(e.message)));
  $("#s-close").addEventListener("click", () => {
    if (app.current && confirm("Close this session? The conversation stays on disk and can be resumed.")) {
      api(`api/sessions/${app.current.key}`, { method: "DELETE" }).catch((e) => toast(e.message));
    }
  });
  $("#s-model").addEventListener("change", (e) => changeSetting("model", e.target.value.trim()));
  $("#s-effort").addEventListener("change", (e) => changeSetting("effort", e.target.value));
  $("#s-mode").addEventListener("change", (e) => changeSetting("mode", e.target.value));
  $("#back").addEventListener("click", () => {
    closeCurrent();
    document.querySelector(".app").classList.remove("in-session");
    $("#session").hidden = true;
    $("#placeholder").hidden = false;
    refreshSessions();
  });
  document.querySelectorAll(".tab").forEach((t) => t.addEventListener("click", () => showTab(t.dataset.tab)));
  await app.providersReady;
  await refreshSessions();
  setInterval(refreshSessions, 3000);
}

init();
