/* RT 远程 web UI */
"use strict";

// ------------------------------------------------------------- helpers ---

const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => [...document.querySelectorAll(sel)];

let TOKEN = localStorage.getItem("rt_token") || "";
const qs = new URLSearchParams(location.search);
if (qs.get("t")) { TOKEN = qs.get("t"); localStorage.setItem("rt_token", TOKEN); history.replaceState(null, "", location.pathname); }

async function api(method, path, body) {
  const opt = { method, headers: {} };
  // Without a stored token rely on the rt_token cookie the server seeded from
  // the ?t= handoff; sending an empty header would break that flow.
  if (TOKEN) opt.headers["X-RT-Token"] = TOKEN;
  if (body !== undefined) {
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opt);
  if (resp.status === 401) { showGate(); throw new Error("unauthorized"); }
  const text = await resp.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch { data = {}; }
  if (!resp.ok) throw new Error(data.error || `HTTP ${resp.status}`);
  return data;
}

function toast(msg, isErr) {
  const t = $("#toast");
  t.textContent = msg;
  t.classList.toggle("error", !!isErr);
  t.classList.remove("hidden");
  clearTimeout(t._tm);
  t._tm = setTimeout(() => t.classList.add("hidden"), 3200);
}

function fmtSize(n) {
  if (n == null || isNaN(n)) return "";
  const k = 1024;
  if (n >= k ** 3) return (n / k ** 3).toFixed(1) + " GB";
  if (n >= k ** 2) return (n / k ** 2).toFixed(1) + " MB";
  if (n >= k) return (n / k).toFixed(1) + " KB";
  return Math.round(n) + " B";
}
function fmtTime(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  return isNaN(d) ? "" : d.toLocaleString("zh-CN", { hour12: false });
}
function fmtAgo(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d)) return "";
  const s = (Date.now() - d.getTime()) / 1000;
  if (s < 0) return "刚刚";
  if (s < 60) return "刚刚";
  if (s < 3600) return Math.floor(s / 60) + " 分钟前";
  if (s < 86400) return Math.floor(s / 3600) + " 小时前";
  if (s < 86400 * 30) return Math.floor(s / 86400) + " 天前";
  return d.toLocaleDateString("zh-CN");
}
function shortID(id) { return id ? id.slice(0, 8) : ""; }
function esc(s) { const d = document.createElement("div"); d.textContent = s ?? ""; return d.innerHTML; }
function statusLabel(st) {
  return { connected: "已连接", connecting: "连接中", online: "在线", reachable: "可连接", offline: "离线" }[st] || st;
}
function platformBadge(platform, version) {
  if (!platform) return "";
  const v = version ? " · v" + esc(version) : "";
  return ` <span class="p-badge">${esc(platform)}${v}</span>`;
}
const CAPS = { term: "⌨ 终端", file: "▣ 文件", fwd: "⇄ 转发" };
function capIcons(caps) {
  return (caps || []).map((c) => (CAPS[c] ? `<span class="cap-badge">${CAPS[c]}</span>` : "")).join("");
}
function viaBadge(conn) {
  if (!conn) return "";
  const relay = conn.kind === "relay";
  return `<span class="v-badge ${relay ? "relay" : "direct"}">${relay ? "中继" : "直连"}</span>`;
}
function peerOnline(id) {
  const p = (state.status?.peers || []).find((x) => x.id === id);
  return !!p && (p.status === "connected" || p.status === "online");
}
function peerName(id) {
  const p = (state.status?.peers || []).find((x) => x.id === id);
  return p ? p.name : shortID(id);
}
function joinPath(base, name) {
  const b = (base || "").replace(/[\\/]+$/, "");
  return b ? b + "/" + name : name;
}
function parentPath(p) {
  const norm = (p || "").replace(/[\\/]+$/, "");
  const i = Math.max(norm.lastIndexOf("/"), norm.lastIndexOf("\\"));
  return i > 0 ? norm.slice(0, i) : i === 0 ? norm.slice(0, 1) : "";
}

// ---------------------------------------------------------------- state ---

const state = {
  status: null,          // /api/status payload
  page: "home",
  device: null,          // device shown on the detail page
  deviceTab: "term",
  fwd: null,             // /api/fwd payload cache
  xfers: new Map(),      // id -> progress
  terms: [],             // {id, peer, session, state, term, ws, fit, pane, retries, ...}
  activeTerm: null,
  termSeq: 0,
  termGone: new Set(),   // "peer:session" the user dismissed; never restored
  activity: [],          // most recent last
};

// File workspaces: one per UI location (files page + device detail page).
// Built in bindWorks() once the DOM is ready.
state.fs = null;
state.dvfs = null;

// ----------------------------------------------------------------- gate ---

function showGate() {
  $("#gate").classList.remove("hidden");
  $("#app").classList.add("hidden");
}
$("#gate-btn").addEventListener("click", () => tryGate());
$("#gate-token").addEventListener("keydown", (e) => { if (e.key === "Enter") tryGate(); });
async function tryGate(silent) {
  TOKEN = $("#gate-token").value.trim() || TOKEN;
  try {
    await api("GET", "/api/status");
    localStorage.setItem("rt_token", TOKEN);
    $("#gate").classList.add("hidden");
    $("#app").classList.remove("hidden");
    boot();
  } catch (e) {
    showGate();
    if (!silent) $("#gate-err").textContent = e.message === "unauthorized" ? "令牌错误" : e.message;
  }
}
// With no stored token the first /api/status call authenticates through the
// rt_token cookie seeded by the server's ?t= redirect; only a 401 falls back
// to the gate (kept free of an error line until the user retries).
if (TOKEN) tryGate(); else tryGate(true);

// ----------------------------------------------------------------- boot ---

let es = null;
function boot() {
  bindWorks();
  buildTermTools($("#term-tools"));
  buildTermTools($("#dv-term-tools"));

  // Restore the view from the URL hash (#/device/<id> survives a refresh).
  const [pg, arg] = parseHash();
  switchPage(pg || "home", pg === "device" ? arg : null);

  refreshStatus();
  connectEvents();
  loadActivity();
  setInterval(() => { if (state.page === "home" || state.page === "device") refreshStatus(); }, 8000);
}

function connectEvents() {
  if (es) es.close();
  es = new EventSource("/api/events?t=" + encodeURIComponent(TOKEN));
  es.onmessage = (m) => {
    let ev; try { ev = JSON.parse(m.data); } catch { return; }
    if (ev.type === "peer_up" || ev.type === "peer_down" || ev.type === "peer_connecting") {
      refreshStatus();
    } else if (ev.type === "xfer") {
      onXferEvent(ev.data);
    } else if (ev.type === "activity") {
      state.activity.push(ev.data);
      if (state.activity.length > 50) state.activity = state.activity.slice(-50);
      renderActivity();
    }
  };
  es.onerror = () => { /* EventSource auto-reconnects */ };
}

async function refreshStatus() {
  try {
    state.status = await api("GET", "/api/status");
    renderSide();
    renderHome();
    if (state.page === "device") renderDeviceHead();
    renderTermPeers();
    renderFsPeers();
    renderFwdPeers();
    renderTermTabs();
    wakeOfflineTerms();
  } catch (e) {
    if (e.message !== "unauthorized") toast(e.message, true);
  }
}

// ----------------------------------------------------------------- nav ---

function parseHash() {
  const h = location.hash.replace(/^#\/?/, "");
  if (h.startsWith("device/")) return ["device", h.slice("device/".length)];
  if (h === "devices") return ["home", null];
  if (["home", "term", "files", "fwd", "settings"].includes(h)) return [h, null];
  return [null, null];
}

// switchPage is the single navigation entry: nav clicks, programmatic jumps
// and hash restoration all land here. The hash is written back so
// #/device/<id> survives refresh; the hashchange listener ignores echoes of
// the state we just applied.
function switchPage(name, arg) {
  if (name === "device") {
    if (arg && arg !== state.device) {
      state.device = arg;
      // New device under inspection: reset its remote pane.
      Object.assign(state.dvfs.remote, { path: "", entries: [], sel: null });
      state.deviceTab = "term";
    }
    if (!state.device) name = "home";
  }
  state.page = name;
  $$(".nav-item").forEach((x) => x.classList.toggle("active", x.dataset.page === name));
  $$(".page").forEach((p) => p.classList.toggle("active", p.id === "page-" + name));
  const wantHash = name === "device" ? "#/device/" + state.device : name === "home" ? "#/" : "#/" + name;
  if (location.hash !== wantHash) location.hash = wantHash;

  if (name === "home") refreshStatus();
  if (name === "fwd") refreshFwd();
  if (name === "files") { renderFsPeers(); loadDirWork(state.fs, "local"); loadDirWork(state.fs, "remote"); }
  if (name === "term") { if (state.activeTerm) activateTerm(state.activeTerm); restoreTerms($("#term-peer").value); }
  if (name === "device") { renderDeviceHead(); setDeviceTab(state.deviceTab); }
}

window.addEventListener("hashchange", () => {
  const [pg, arg] = parseHash();
  if (!pg) return;
  if (pg === state.page && (pg !== "device" || arg === state.device)) return;
  switchPage(pg, pg === "device" ? arg : null);
});

function renderSide() {
  const s = state.status; if (!s) return;
  $("#side-name").textContent = s.name;
  $("#side-name").title = s.name;
  $("#side-id").textContent = s.id;
  $("#set-id").textContent = s.id;
  $("#set-name").value = s.name;
  if (document.activeElement !== $("#set-hub")) $("#set-hub").value = s.hub_addr || "";
}

// ---------------------------------------------------------- home：总览 ---

function renderHome() {
  renderStats();
  renderInvite();
  renderDeviceGrid();
  renderDiscover();
  renderActivity();
}

function renderStats() {
  const peers = state.status?.peers || [];
  const online = peers.filter((p) => p.status === "connected" || p.status === "online").length;
  const sessions = state.terms.filter((t) => t.state !== "dead").length;
  const xf = [...state.xfers.values()].filter((x) => x.state === "running").length;
  $("#st-devices").textContent = peers.length;
  $("#st-online").textContent = online;
  $("#st-sessions").textContent = sessions;
  $("#st-xfers").textContent = xf;
}

function renderInvite() {
  const s = state.status; if (!s) return;
  const box = $("#invite-box");
  if (s.invite) {
    const left = Math.max(0, Math.round((s.invite.expires - Date.now()) / 1000));
    box.innerHTML = `<div class="invite-code">${esc(s.invite.code)}</div>
      <div class="invite-sub">${left > 0 ? left + " 秒后过期" : "已过期，请重新生成"}</div>
      <button class="btn small" id="invite-re" style="margin-top:8px">重新生成</button>`;
    $("#invite-re").addEventListener("click", makeInvite);
  } else {
    box.innerHTML = `<button id="invite-btn" class="btn primary">生成配对码</button>`;
    $("#invite-btn").addEventListener("click", makeInvite);
  }
}

function renderDeviceGrid() {
  const grid = $("#device-grid");
  if (!state.status) { grid.innerHTML = `<div class="empty">加载中…</div>`; return; }
  const peers = state.status.peers || [];
  grid.innerHTML = "";
  if (!peers.length) {
    grid.innerHTML = `<div class="empty">还没有配对设备。用下方配对码完成首次配对。</div>`;
    return;
  }
  for (const p of peers) {
    const st = p.status || "offline";
    const card = document.createElement("div");
    card.className = "device-card st-" + st;
    card.innerHTML = `
      <div class="dc-top">
        <span class="dot"></span>
        <b class="dc-name">${esc(p.name)}</b>
        ${platformBadge(p.platform, p.version)}
        ${viaBadge(p.conn)}
      </div>
      <div class="dc-caps">${capIcons(p.caps)}</div>
      <div class="dc-meta mono">${shortID(p.id)}${p.last_addr ? " · " + esc(p.last_addr) : ""}</div>
      <div class="dc-meta dim">${p.last_seen ? "最近在线 " + fmtAgo(p.last_seen) : "从未连接"} · ${esc(statusLabel(st))}</div>
      <div class="dc-actions"></div>`;
    // Clicking the card opens the device detail page; buttons keep their own
    // handlers.
    card.addEventListener("click", (e) => { if (e.target.closest("button")) return; switchPage("device", p.id); });
    const act = card.querySelector(".dc-actions");
    const on = st === "connected";
    const btn = document.createElement("button");
    btn.className = "btn small" + (on ? "" : " primary");
    btn.textContent = on ? "断开" : "连接";
    btn.addEventListener("click", async () => {
      try {
        await api("POST", on ? "/api/peers/disconnect" : "/api/peers/connect", { id: p.id });
        refreshStatus();
      } catch (e) { toast(e.message, true); }
    });
    act.appendChild(btn);
    const btnT = document.createElement("button");
    btnT.className = "btn small";
    btnT.textContent = "终端";
    btnT.addEventListener("click", () => { openTerm(p.id); switchPage("term"); });
    act.appendChild(btnT);
    const btnF = document.createElement("button");
    btnF.className = "btn small";
    btnF.textContent = "文件";
    btnF.addEventListener("click", () => { switchPage("device", p.id); setDeviceTab("files"); });
    act.appendChild(btnF);
    grid.appendChild(card);
  }
}

function renderActivity() {
  const list = $("#activity-list");
  if (!list) return;
  const items = state.activity.slice().reverse();
  if (!items.length) { list.innerHTML = `<div class="empty">暂无记录</div>`; return; }
  list.innerHTML = "";
  for (const a of items) {
    const row = document.createElement("div");
    row.className = "act-row act-" + esc(a.kind || "peer");
    row.innerHTML = `<span class="act-time mono">${fmtTime(a.time)}</span><span class="act-text">${esc(a.text)}</span>`;
    list.appendChild(row);
  }
}

async function loadActivity() {
  try {
    const res = await api("GET", "/api/activity?limit=50");
    state.activity = res.items || [];
    renderActivity();
  } catch { /* older backend without the endpoint: keep the empty state */ }
}

// LAN discovery rows: unpaired devices seen via mDNS; "填入配对" copies the
// address into the pairing form.
function renderDiscover() {
  const s = state.status;
  const dl = $("#discover-list");
  if (!s) return;
  dl.innerHTML = "";
  if (!(s.discovered || []).length) {
    dl.innerHTML = `<div class="empty">同一局域网内未发现其他设备（双方都需运行 rtd）。</div>`;
    return;
  }
  for (const d of s.discovered || []) {
    const row = document.createElement("div");
    row.className = "peer-row";
    row.innerHTML = `
      <span class="dot" style="background:var(--acc)"></span>
      <div class="p-name">${esc(d.name || "未命名")}${platformBadge(d.platform, "")}</div>
      <div class="p-meta mono">${shortID(d.id)} · ${esc(d.addr)}</div>
      <div class="p-actions"><button class="btn small primary">填入配对</button></div>`;
    row.querySelector("button").addEventListener("click", () => {
      $("#pair-addr").value = d.addr;
      $("#pair-code").focus();
    });
    dl.appendChild(row);
  }
}

async function makeInvite() {
  try {
    await api("POST", "/api/pair/invite", {});
    refreshStatus();
  } catch (e) { toast(e.message, true); }
}

$("#pair-btn").addEventListener("click", async () => {
  const addr = $("#pair-addr").value.trim();
  const code = $("#pair-code").value.trim();
  const msg = $("#pair-msg");
  msg.textContent = "配对中…";
  try {
    const res = await api("POST", "/api/pair/connect", { addr, code });
    msg.textContent = "";
    toast("已配对 " + (res.peer?.name || ""));
    $("#pair-addr").value = ""; $("#pair-code").value = "";
    refreshStatus();
  } catch (e) { msg.textContent = ""; toast(e.message, true); }
});

// ----------------------------------------------------- device 详情页 ---

function renderDeviceHead() {
  const head = $("#device-head");
  if (!state.status) { head.innerHTML = `<div class="card"><div class="empty">加载中…</div></div>`; return; }
  const p = state.status.peers.find((x) => x.id === state.device);
  if (!p) {
    head.innerHTML = `<div class="card"><div class="empty">设备不存在或已删除。</div></div>`;
    return;
  }
  const st = p.status || "offline";
  const on = st === "connected";
  const offlineHint = on ? "" : `<div class="warn-bar">设备当前未连接（${esc(statusLabel(st))}），终端、文件与端口转发操作不可用。${st === "reachable" ? "点击上方「连接」或等待设备上线。" : ""}</div>`;
  head.innerHTML = `${offlineHint}
    <div class="card dh st-${st}">
      <button class="icon-btn dh-back" title="返回首页">←</button>
      <span class="dot"></span>
      <b class="dh-name">${esc(p.name)}</b>
      ${platformBadge(p.platform, p.version)}
      ${viaBadge(p.conn)}
      <span class="dh-caps">${capIcons(p.caps)}</span>
      <span class="dh-meta dim mono">${shortID(p.id)}${p.last_addr ? " · " + esc(p.last_addr) : ""}</span>
      <span class="dh-meta dim">最近在线 ${fmtAgo(p.last_seen) || "从未"} · ${esc(statusLabel(st))}</span>
      <span class="dh-actions">
        <button class="btn small ${on ? "" : "primary"}" data-act="conn">${on ? "断开" : "连接"}</button>
        <button class="btn small danger" data-act="del">删除</button>
      </span>
    </div>`;
  head.querySelector(".dh-back").addEventListener("click", () => switchPage("home"));
  head.querySelector('[data-act="conn"]').addEventListener("click", async () => {
    try {
      await api("POST", on ? "/api/peers/disconnect" : "/api/peers/connect", { id: p.id });
      refreshStatus();
    } catch (e) { toast(e.message, true); }
  });
  head.querySelector('[data-act="del"]').addEventListener("click", async () => {
    if (!confirm(`删除配对 ${p.name} (${shortID(p.id)})？`)) return;
    try { await api("POST", "/api/peers/remove", { id: p.id }); switchPage("home"); refreshStatus(); } catch (e) { toast(e.message, true); }
  });
}

$$("#device-tabs button").forEach((b) => b.addEventListener("click", () => setDeviceTab(b.dataset.tab)));

function setDeviceTab(tab) {
  state.deviceTab = tab;
  $$("#device-tabs button").forEach((b) => b.classList.toggle("active", b.dataset.tab === tab));
  $$(".dv-tab").forEach((el) => el.classList.toggle("active", el.id === "dv-tab-" + tab));
  $("#dv-term-info").textContent = peerName(state.device) + " 的终端会话";
  if (tab === "term") {
    const mine = state.terms.filter((t) => t.peer === state.device && t.state !== "dead");
    if (mine.length && !mine.includes(state.activeTerm)) activateTerm(mine[mine.length - 1]);
    renderTermTabs();
    restoreTerms(state.device);
  }
  if (tab === "files") { loadDirWork(state.dvfs, "local"); loadDirWork(state.dvfs, "remote"); }
  if (tab === "fwd") {
    if (state.fwd) renderFwdInto($("#dv-fwd-list")); else refreshFwd();
  }
}

$("#dv-term-open").addEventListener("click", () => openTerm(state.device));
$("#dv-fwd-add").addEventListener("click", () => {
  switchPage("fwd");
  $("#fwd-peer").value = state.device || "";
});

// ---------------------------------------------------------------- term ---
// Sessions live in state.terms and are shared between the terminal page and
// the device detail page: tabs re-render into both containers, and
// activateTerm moves the pane DOM into whichever container is on screen.

const TERM_THEMES = {
  dark: { name: "暗色", theme: { background: "#0b0e14", foreground: "#dfe6f3", cursor: "#4f7cff", cursorAccent: "#0b0e14", selectionBackground: "rgba(79,124,255,.35)" } },
  light: { name: "亮色", theme: { background: "#f5f6f8", foreground: "#24292f", cursor: "#3b63d8", cursorAccent: "#f5f6f8", selectionBackground: "rgba(59,99,216,.25)" } },
  solar: { name: "Solarized", theme: { background: "#002b36", foreground: "#c8d5dd", cursor: "#93a1a1", cursorAccent: "#002b36", selectionBackground: "rgba(147,161,161,.3)" } },
  mono: { name: "单色", theme: { background: "#000000", foreground: "#e0e0e0", cursor: "#ffffff", cursorAccent: "#000000", selectionBackground: "rgba(255,255,255,.3)" } },
};

let termPrefs = loadTermPrefs();
function loadTermPrefs() {
  try {
    const p = JSON.parse(localStorage.getItem("rt_term_prefs") || "{}");
    return {
      fontSize: Math.min(24, Math.max(9, Number(p.fontSize) || 13.5)),
      theme: TERM_THEMES[p.theme] ? p.theme : "dark",
    };
  } catch { return { fontSize: 13.5, theme: "dark" }; }
}
function saveTermPrefs() { localStorage.setItem("rt_term_prefs", JSON.stringify(termPrefs)); }

// applyTermOpt works across xterm.js versions (setOption pre-v4, options
// object afterwards).
function applyTermOpt(term, key, val) {
  try {
    if (typeof term.setOption === "function") term.setOption(key, val);
    else term.options[key] = val;
  } catch { /* ignore */ }
}

function applyTermPrefs() {
  const theme = TERM_THEMES[termPrefs.theme]?.theme;
  for (const ts of state.terms) {
    applyTermOpt(ts.term, "fontSize", termPrefs.fontSize);
    if (theme) applyTermOpt(ts.term, "theme", theme);
    try { ts.fit.fit(); } catch { /* pane may be hidden */ }
  }
  $$(".term-theme").forEach((s) => { s.value = termPrefs.theme; });
}

function buildTermTools(host) {
  host.innerHTML = `
    <button class="icon-btn" data-t="copy" title="复制选中内容 (Ctrl+Shift+C)">⧉</button>
    <button class="icon-btn" data-t="paste" title="粘贴 (Ctrl+V)">📋</button>
    <button class="icon-btn" data-t="clear" title="清屏 (Ctrl+L)">⌫</button>
    <button class="icon-btn" data-t="font-" title="减小字号">A−</button>
    <button class="icon-btn" data-t="font+" title="增大字号">A＋</button>
    <select class="term-theme" title="终端主题">${Object.entries(TERM_THEMES).map(([k, v]) => `<option value="${k}">${v.name}</option>`).join("")}</select>`;
  host.querySelectorAll("button").forEach((b) => b.addEventListener("click", () => termTool(b.dataset.t)));
  const sel = host.querySelector(".term-theme");
  sel.value = termPrefs.theme;
  sel.addEventListener("change", () => { termPrefs.theme = sel.value; saveTermPrefs(); applyTermPrefs(); });
}

function termTool(t) {
  const ts = state.activeTerm;
  if ((t === "copy" || t === "paste" || t === "clear") && !ts) { toast("没有活跃的终端会话", true); return; }
  if (t === "copy") copySel(ts);
  else if (t === "paste") pasteClip(ts);
  else if (t === "clear") clearTerm(ts);
  else if (t === "font-") { termPrefs.fontSize = Math.max(9, termPrefs.fontSize - 1); saveTermPrefs(); applyTermPrefs(); }
  else if (t === "font+") { termPrefs.fontSize = Math.min(24, termPrefs.fontSize + 1); saveTermPrefs(); applyTermPrefs(); }
}

function copyTextFallback(text) {
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed"; ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand("copy"); toast("已复制"); } catch { toast("复制失败，请手动复制", true); }
  ta.remove();
}

async function copySel(ts) {
  if (!ts.term.hasSelection()) { toast("请先用鼠标选中要复制的文本", true); return; }
  const text = ts.term.getSelection();
  try { await navigator.clipboard.writeText(text); toast("已复制"); }
  catch { copyTextFallback(text); }
}

async function pasteClip(ts) {
  let text = "";
  try { text = await navigator.clipboard.readText(); }
  catch { text = prompt("浏览器拒绝了剪贴板读取，请手动粘贴内容：") || ""; }
  if (text) ts.term.paste(text);
}

function clearTerm(ts) {
  ts.term.clear();
  // Ctrl+L so the remote shell redraws its prompt on a clean line.
  if (ts.ws && ts.ws.readyState === 1) ts.ws.send("\x0c");
}

function renderTermPeers() {
  const sel = $("#term-peer");
  const cur = sel.value;
  sel.innerHTML = (state.status?.peers || []).map((p) => `<option value="${esc(p.id)}">${esc(p.name)}</option>`).join("");
  if (cur && [...sel.options].some((o) => o.value === cur)) sel.value = cur;
}

$("#term-open").addEventListener("click", () => openTerm($("#term-peer").value));

function termPanesHost() {
  return state.page === "device" && state.deviceTab === "term" ? $("#dv-term-panes") : $("#term-panes");
}

function openTerm(peerID, sessionID) {
  if (!peerID) { toast("请先选择设备", true); return; }
  const p = (state.status?.peers || []).find((x) => x.id === peerID);
  if (p && p.status === "offline") { toast("设备离线，请先连接", true); return; }
  const id = "t" + (++state.termSeq);
  const pane = document.createElement("div");
  pane.className = "term-pane";
  const host = document.createElement("div");
  pane.appendChild(host);
  termPanesHost().appendChild(pane);

  const term = new Terminal({
    fontFamily: 'Consolas, "Cascadia Mono", monospace',
    fontSize: termPrefs.fontSize,
    cursorBlink: true,
    theme: TERM_THEMES[termPrefs.theme].theme,
    scrollback: 5000,
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(host);

  const cols = fit.proposeDimensions()?.cols || 80;
  const rows = fit.proposeDimensions()?.rows || 24;
  // sessionID re-attaches to a PTY that is still alive on the peer (tab
  // restore after a refresh); 0 opens a fresh session.
  const ts = { id, peer: peerID, session: sessionID || 0, state: "attaching", term, ws: null, fit, pane, name: peerName(peerID), retries: 0, reconnectTm: 0, manualClose: false };
  pane._ts = ts;
  state.terms.push(ts);

  dialTerm(ts, cols, rows);
  renderTermTabs();
  activateTerm(ts);
}

// restoreTerms brings back tabs for sessions that are still alive on the peer
// but unknown to this browser (e.g. after a page refresh). Sessions the user
// dismissed (✕ on a detached tab) are never restored again.
async function restoreTerms(peerID) {
  if (!peerID || !peerOnline(peerID)) return;
  let res;
  try { res = await api("GET", "/api/term/list?peer=" + encodeURIComponent(peerID)); }
  catch { return; } // older backend without the endpoint: nothing to restore
  for (const info of res.sessions || []) {
    // A session with a live viewer (rt term, another tab) must not be stolen:
    // attaching replaces the stream and drops the current viewer, so only
    // orphaned sessions are restored.
    if (info.attached) continue;
    if (state.terms.some((t) => t.peer === peerID && t.session === info.session)) continue;
    if (state.termGone.has(peerID + ":" + info.session)) continue;
    openTerm(peerID, info.session);
  }
}

function dialTerm(ts, cols, rows) {
  const url = `ws://${location.host}/api/term/ws?peer=${encodeURIComponent(ts.peer)}&cols=${cols}&rows=${rows}` +
    (ts.session ? `&session=${ts.session}` : "") +
    `&t=${encodeURIComponent(TOKEN)}`;
  let ws;
  try { ws = new WebSocket(url); } catch (e) { toast(e.message, true); markTermDead(ts, "无法连接"); return; }
  ts.ws = ws;
  ws.binaryType = "arraybuffer";
  ws.onopen = () => {
    if (ts.retries > 0) ts.term.write("\r\n\x1b[32m— 已重新连接 —\x1b[0m\r\n");
    ts.retries = 0;
    setTermState(ts, "online");
    ts.term.focus();
    try { ts.fit.fit(); } catch { /* not laid out yet */ }
  };
  ws.onmessage = (m) => {
    if (typeof m.data === "string") {
      // Control frames (JSON): the session id announcement, or a dial error —
      // an error marks the tab dead instead of looping reconnects against a
      // session that no longer exists.
      try {
        const j = JSON.parse(m.data);
        if (j.session) ts.session = j.session;
        if (j.error) { markTermDead(ts, j.error); return; }
      } catch { /* not json */ }
      return;
    }
    ts.term.write(new Uint8Array(m.data));
  };
  ws.onclose = () => {
    // detachTerm/endTermSession/markTermDead already disposed the UI side.
    if (ts.manualClose || ts.state === "dead" || !state.terms.includes(ts)) return;
    scheduleTermReconnect(ts);
  };
  // Report PTY resizes back: the server accepts {"cols":N,"rows":N} text
  // frames, so window resizes and tab switches reach the remote shell.
  ts.term.onResize(({ cols: c, rows: r }) => {
    if (ts.ws && ts.ws.readyState === 1) ts.ws.send(JSON.stringify({ cols: c, rows: r }));
  });
  ts.term.onData((d) => { if (ts.ws && ts.ws.readyState === 1) ts.ws.send(d); });
  ts.term.attachCustomKeyEventHandler((e) => {
    if (e.type !== "keydown") return true;
    const k = (e.key || "").toLowerCase();
    if (e.ctrlKey && e.shiftKey && k === "c") { copySel(ts); return false; }
    if (e.ctrlKey && !e.shiftKey && k === "insert") { copySel(ts); return false; }
    // Ctrl+V and Ctrl+Shift+V both paste; Shift+Insert too.
    if (e.ctrlKey && k === "v") { pasteClip(ts); return false; }
    if (e.shiftKey && k === "insert") { pasteClip(ts); return false; }
    return true;
  });
}

function setTermState(ts, st) {
  ts.state = st;
  renderTermTabs();
  renderStats();
}

function scheduleTermReconnect(ts) {
  clearTimeout(ts.reconnectTm);
  setTermState(ts, "reconnecting");
  ts.term.write("\r\n\x1b[90m— 连接断开，正在重连… —\x1b[0m\r\n");
  const delay = [3000, 5000, 10000][Math.min(ts.retries, 2)];
  ts.retries += 1;
  ts.reconnectTm = setTimeout(() => reconnectTerm(ts), delay);
}

function reconnectTerm(ts) {
  if (ts.manualClose || ts.state === "dead") return;
  if (!peerOnline(ts.peer) || ts.retries > 3) {
    // Stop dialing until the device shows up again (wakeOfflineTerms on
    // refreshStatus, or a click on the tab).
    if (ts.ws && ts.ws.readyState === 1) return;
    setTermState(ts, "offline");
    return;
  }
  const dims = ts.fit.proposeDimensions() || { cols: 80, rows: 24 };
  dialTerm(ts, dims.cols, dims.rows);
}

// wakeOfflineTerms resumes sessions whose device came back online.
function wakeOfflineTerms() {
  for (const ts of state.terms) {
    if (ts.state === "offline" && peerOnline(ts.peer)) {
      ts.retries = 0;
      reconnectTerm(ts);
    }
  }
}

function markTermDead(ts, why) {
  clearTimeout(ts.reconnectTm);
  ts.state = "dead";
  // The tab may have been disposed while the error frame was in flight.
  try { ts.term.write(`\r\n\x1b[90m— 会话已结束${why ? "（" + why + "）" : ""} —\x1b[0m\r\n`); } catch { /* disposed */ }
  renderTermTabs();
  renderStats();
}

function renderTermTabs() {
  renderTabsInto($("#term-tabs"), state.terms);
  renderTabsInto($("#dv-term-tabs"), state.terms.filter((t) => t.peer === state.device));
}

function renderTabsInto(hostEl, list) {
  if (!hostEl) return;
  hostEl.innerHTML = "";
  for (const ts of list) {
    const tab = document.createElement("div");
    const badge = { online: "在线", reconnecting: "重连中", offline: "设备离线", dead: "已结束", attaching: "连接中" }[ts.state] || "";
    tab.className = "term-tab ts-" + ts.state + (state.activeTerm === ts ? " active" : "");
    tab.innerHTML = `<span class="t-dot"></span><span class="t-name">${esc(ts.name)}${ts.session ? " #" + ts.session : ""}</span>` +
      (badge ? `<span class="t-st">${badge}</span>` : "") +
      `<span class="x">✕</span>`;
    tab.addEventListener("click", (e) => {
      if (e.target.classList.contains("x")) return; // handled by the ✕ binding
      activateTerm(ts);
      if (ts.state === "offline" && peerOnline(ts.peer)) { ts.retries = 0; reconnectTerm(ts); }
    });
    tab.querySelector(".x").addEventListener("click", (e) => { e.stopPropagation(); closeTerm(ts); });
    hostEl.appendChild(tab);
  }
}

function activateTerm(ts) {
  // Move the pane into whichever container is currently on screen.
  const host = termPanesHost();
  if (ts.pane.parentNode !== host) host.appendChild(ts.pane);
  $$(".term-pane").forEach((p) => p.classList.remove("active"));
  ts.pane.classList.add("active");
  state.activeTerm = ts;
  renderTermTabs();
  refreshTermEmpty();
  requestAnimationFrame(() => { try { ts.fit.fit(); ts.term.focus(); } catch { /* hidden */ } });
}

function refreshTermEmpty() {
  [$("#term-panes"), $("#dv-term-panes")].forEach((hostEl) => {
    const empty = hostEl.querySelector(".term-empty");
    if (empty) empty.style.display = hostEl.querySelector(".term-pane") ? "none" : "";
  });
}

function closeTerm(ts) {
  const live = ts.ws && ts.ws.readyState === 1 && ts.state !== "dead";
  if (live) {
    if (confirm("仅断开此标签？远端会话将继续在后台运行，可随时重新连接。")) {
      detachTerm(ts);
      return;
    }
    if (confirm("改为结束远端会话？远端 shell 将被终止，无法恢复。")) {
      endTermSession(ts);
      return;
    }
    return; // user cancelled both
  }
  removeTermTab(ts);
}

// detachTerm closes only the browser side: the remote PTY keeps running and
// the session can be re-attached later (ring buffer replay).
function detachTerm(ts) {
  ts.manualClose = true;
  clearTimeout(ts.reconnectTm);
  try { ts.ws.close(); } catch { /* already closed */ }
  removeTermTab(ts);
}

async function endTermSession(ts) {
  ts.manualClose = true;
  clearTimeout(ts.reconnectTm);
  try { ts.ws.close(); } catch { /* already closed */ }
  if (ts.session) {
    try { await api("POST", "/api/term/close", { peer: ts.peer, session: ts.session }); }
    catch (e) { toast(e.message, true); }
  }
  removeTermTab(ts);
}

function removeTermTab(ts) {
  clearTimeout(ts.reconnectTm);
  try { ts.term.dispose(); } catch { /* already disposed */ }
  ts.pane.remove();
  state.terms = state.terms.filter((x) => x !== ts);
  // A dismissed tab whose remote session may still be alive must not pop up
  // again on the next page visit.
  if (ts.session) state.termGone.add(ts.peer + ":" + ts.session);
  if (state.activeTerm === ts) {
    state.activeTerm = null;
    const rest = state.terms.filter((t) => t.state !== "dead");
    if (rest.length) activateTerm(rest[rest.length - 1]);
  }
  renderTermTabs();
  refreshTermEmpty();
  renderStats();
}

function fitTerm(ts) { try { ts.fit.fit(); } catch { /* hidden */ } }

window.addEventListener("resize", () => { if (state.activeTerm) fitTerm(state.activeTerm); });

// ---------------------------------------------------------------- files ---
// Two workspaces share the same rendering code: the files page binds its DOM
// to state.fs (peer from the select), the device detail page binds its DOM to
// state.dvfs (peer = the device being inspected).

function bindWorks() {
  state.fs = makeWork($("#files-root"), () => $("#fs-peer").value);
  state.dvfs = makeWork($("#dv-files-root"), () => state.device);
}

function makeWork(rootEl, peerFn) {
  const work = {
    peerFn,
    local: { path: "", entries: [], sel: null, el: null },
    remote: { path: "", entries: [], sel: null, el: null },
  };
  rootEl.querySelectorAll(".pane").forEach((pane) => bindFilePane(pane, work));
  const mid = rootEl.querySelector(".files-mid");
  work.midPush = mid.querySelector(".mid-push");
  work.midPull = mid.querySelector(".mid-pull");
  work.midPush.addEventListener("click", () => midTransfer(work, "local"));
  work.midPull.addEventListener("click", () => midTransfer(work, "remote"));
  return work;
}

// panePeer is the peer argument for fs operations on one pane: the local pane
// addresses this daemon (peer ""), not the selected remote peer.
function panePeer(work, side) {
  return side === "local" ? "" : work.peerFn();
}

function bindFilePane(pane, work) {
  const side = pane.dataset.side;
  const st = work[side];
  st.pathEl = pane.querySelector(".pane-path");
  st.listEl = pane.querySelector(".pane-list");
  pane.querySelectorAll(".pane-actions .icon-btn").forEach((b) => {
    b.addEventListener("click", async () => {
      const act = b.dataset.act;
      if (act === "up") {
        st.path = parentPath(st.path);
        loadDirWork(work, side);
      } else if (act === "refresh") {
        loadDirWork(work, side);
      } else if (act === "mkdir") {
        const n = prompt("文件夹名");
        if (!n) return;
        try {
          await api("POST", "/api/fs/mkdir", { peer: panePeer(work, side), path: joinPath(st.path, n) });
          loadDirWork(work, side);
        } catch (e) { toast(e.message, true); }
      }
    });
  });
  // Browser drag & drop cannot hand file contents to the daemon (no upload
  // channel yet) — explain instead of silently doing the wrong thing.
  if (side === "remote") {
    pane.addEventListener("dragover", (e) => { e.preventDefault(); pane.classList.add("dropping"); });
    pane.addEventListener("dragleave", () => pane.classList.remove("dropping"));
    pane.addEventListener("drop", (e) => {
      e.preventDefault();
      pane.classList.remove("dropping");
      toast("浏览器直传将在后续版本支持：请选中文件后点击「发送」。");
    });
  }
}

async function loadDirWork(work, side) {
  const st = work[side];
  const peer = side === "remote" ? work.peerFn() : "";
  if (side === "remote" && !peer) {
    st.pathEl.textContent = "先选择远端设备";
    st.entries = [];
    renderDir(work, side);
    return;
  }
  if (side === "remote" && !peerOnline(peer)) {
    // Fail with a clear reason instead of a bare RPC error: fs browsing
    // needs a live session, which only "connected" devices have.
    const p = (state.status?.peers || []).find((x) => x.id === peer);
    st.pathEl.textContent = p ? `设备未连接（${statusLabel(p.status)}），连接后可浏览文件` : "设备不存在";
    st.entries = [];
    renderDir(work, side);
    return;
  }
  st.listEl.innerHTML = `<div class="empty" style="padding:12px">加载中…</div>`;
  try {
    const res = await api("GET", `/api/fs/list?peer=${encodeURIComponent(peer)}&path=${encodeURIComponent(st.path)}`);
    st.path = res.path || "";
    st.entries = res.entries || [];
    renderDir(work, side);
  } catch (e) {
    if (e.message !== "unauthorized") st.pathEl.textContent = "错误: " + e.message;
  }
}

function renderDir(work, side) {
  const st = work[side];
  const listEl = st.listEl;
  st.pathEl.textContent = st.path || (side === "local" ? "本机" : "此电脑");
  listEl.innerHTML = "";
  for (const en of st.entries) {
    const row = document.createElement("div");
    row.className = "f-row " + (en.dir ? "dir" : "file");
    const selected = st.sel === en.name;
    if (selected) row.style.background = "rgba(79,124,255,.15)";
    row.innerHTML = `<span class="f-ic">${en.dir ? "▸" : "▪"}</span>
      <span class="f-name" title="${esc(en.name)}">${esc(en.name)}</span>
      <span class="f-size">${en.dir ? "" : fmtSize(en.size)}</span>
      <span class="f-act"></span>`;
    row.querySelector(".f-name").addEventListener("click", () => {
      if (en.dir) {
        st.path = joinPath(st.path, en.name);
        loadDirWork(work, side);
      } else {
        st.sel = en.name;
        renderDir(work, side);
      }
    });
    const act = row.querySelector(".f-act");
    if (!en.dir) {
      const send = document.createElement("button");
      send.className = "icon-btn";
      send.title = side === "local" ? "发送到远端" : "下载到本机";
      send.textContent = side === "local" ? "⇒" : "⇐";
      send.addEventListener("click", () => transferFile(work, side, en.name));
      act.appendChild(send);
    }
    const ren = document.createElement("button");
    ren.className = "icon-btn"; ren.title = "重命名"; ren.textContent = "✎";
    ren.addEventListener("click", async () => {
      const nn = prompt("重命名", en.name);
      if (!nn || nn === en.name) return;
      try {
        await api("POST", "/api/fs/rename", { peer: panePeer(work, side), from: joinPath(st.path, en.name), to: joinPath(st.path, nn) });
        loadDirWork(work, side);
      } catch (e) { toast(e.message, true); }
    });
    const del = document.createElement("button");
    del.className = "icon-btn"; del.title = "删除"; del.textContent = "✕";
    del.addEventListener("click", async () => {
      const sideName = side === "local" ? "本机" : "远端";
      if (!confirm(`删除 ${sideName} ${en.name}${en.dir ? "（含内容）" : ""}？`)) return;
      try {
        await api("POST", "/api/fs/remove", { peer: panePeer(work, side), path: joinPath(st.path, en.name), recursive: !!en.dir });
        loadDirWork(work, side);
      } catch (e) { toast(e.message, true); }
    });
    act.appendChild(ren); act.appendChild(del);
    listEl.appendChild(row);
  }
  if (!st.entries.length) listEl.innerHTML = `<div class="empty" style="padding:12px">空目录</div>`;
  updateMid(work);
}

function selEntry(work, side) {
  const st = work[side];
  return st.entries.find((e) => e.name === st.sel);
}

function updateMid(work) {
  const l = selEntry(work, "local");
  const r = selEntry(work, "remote");
  work.midPush.disabled = !(l && !l.dir);
  work.midPull.disabled = !(r && !r.dir);
}

function midTransfer(work, side) {
  const en = selEntry(work, side);
  if (!en || en.dir) return;
  transferFile(work, side, en.name);
}

function transferFile(work, side, name) {
  const peer = work.peerFn();
  if (!peer) { toast("请先选择远端设备", true); return; }
  const remotePath = joinPath(work.remote.path, name);
  const localPath = joinPath(work.local.path, name);
  (async () => {
    try {
      if (side === "local") {
        await api("POST", "/api/xfer/push", { peer, local: localPath, remote: remotePath, conflict: "rename" });
      } else {
        await api("POST", "/api/xfer/pull", { peer, remote: remotePath, local: localPath, conflict: "rename" });
      }
      toast("已开始传输: " + name);
    } catch (e) { toast(e.message, true); }
  })();
}

function renderFsPeers() {
  const sel = $("#fs-peer");
  const cur = sel.value;
  sel.innerHTML = (state.status?.peers || []).map((p) => `<option value="${esc(p.id)}">${esc(p.name)}</option>`).join("");
  if (cur && [...sel.options].some((o) => o.value === cur)) sel.value = cur;
}

$("#fs-peer").addEventListener("change", () => {
  Object.assign(state.fs.remote, { path: "", entries: [], sel: null });
  loadDirWork(state.fs, "remote");
});

// transfers ----------------------------------------------------------

function onXferEvent(p) {
  if (!p || !p.id) return;
  const prev = state.xfers.get(p.id);
  if (prev && p.state === "running" && prev.state === "running" && prev._t) {
    const dt = (Date.now() - prev._t) / 1000;
    if (dt > 0) {
      const sp = Math.max(0, (p.done - prev.done) / dt);
      p._speed = sp > 0 ? sp : prev._speed;
      p._t = Date.now();
    } else {
      p._speed = prev._speed; p._t = prev._t;
    }
  } else {
    p._speed = 0;
    p._t = Date.now();
  }
  state.xfers.set(p.id, p);
  renderXfers();
}

function renderXfers() {
  const list = $("#xfer-list");
  list.innerHTML = "";
  const entries = [...state.xfers.entries()].slice(-30);
  if (!entries.length) { list.innerHTML = `<div class="empty">暂无传输任务</div>`; }
  for (const [, p] of entries) {
    const row = document.createElement("div");
    row.className = "xfer-row";
    const pct = p.size > 0 ? Math.min(100, Math.round((100 * p.done) / p.size)) : 0;
    const canceled = p.state === "error" && p.err === "canceled";
    const stTxt = p.state === "running"
      ? fmtSize(p.done) + (p.size ? " / " + fmtSize(p.size) : "") + (p._speed ? " · " + fmtSize(p._speed) + "/s" : "")
      : canceled ? "已取消"
      : p.state === "done" ? "已完成"
      : p.state === "interrupted" ? "已中断（重试可续传）"
      : p.state + (p.err ? ": " + esc(p.err) : "");
    row.innerHTML = `
      <div class="x-top">
        <span class="x-name">${p.direction === "push" ? "↑" : "↓"} ${esc(p.name || p.local_path)}</span>
        <span class="x-stwrap">
          <span class="x-state ${canceled ? "canceled" : p.state}">${stTxt}</span>
          ${p.state === "running" ? `<button class="icon-btn x-cancel" title="取消传输">✕</button>` : ""}
        </span>
      </div>
      <div class="xbar"><div style="width:${p.state === "done" ? 100 : pct}%"></div></div>`;
    if (p.state === "running") row.querySelector(".x-cancel").addEventListener("click", () => cancelXfer(p.id));
    list.appendChild(row);
  }
  const running = [...state.xfers.values()].filter((x) => x.state === "running").length;
  $("#fs-progress-summary").textContent = running ? `${running} 个传输中` : "";
  $("#dv-xfer-summary").textContent = running ? `${running} 个传输中（详见「文件」页）` : "";
  renderStats();
}

async function cancelXfer(id) {
  try { await api("POST", "/api/xfer/cancel", { id }); } catch (e) { toast(e.message, true); }
}

$("#xfer-clear").addEventListener("click", () => { state.xfers.clear(); renderXfers(); });

// ----------------------------------------------------------------- fwd ---

function renderFwdPeers() {
  const sel = $("#fwd-peer");
  const cur = sel.value;
  sel.innerHTML = (state.status?.peers || []).map((p) => `<option value="${esc(p.id)}">${esc(p.name)}</option>`).join("");
  if (cur && [...sel.options].some((o) => o.value === cur)) sel.value = cur;
}

async function refreshFwd() {
  renderFwdPeers();
  try {
    state.fwd = await api("GET", "/api/fwd");
  } catch (e) { toast(e.message, true); return; }
  renderFwdInto($("#fwd-list"));
  renderFwdInto($("#dv-fwd-list"));
}

function renderFwdInto(listEl) {
  const data = state.fwd;
  if (!data || !listEl) return;
  listEl.innerHTML = "";
  const rules = data.rules || [];
  if (!rules.length) { listEl.innerHTML = `<div class="empty">暂无规则。连接设备后用上方表单添加。</div>`; return; }
  const tbl = document.createElement("table");
  tbl.className = "fwd-table";
  tbl.innerHTML = `
    <thead><tr><th>名称</th><th>类型</th><th>本地地址</th><th>远程地址</th><th>状态</th><th>启动时间</th><th></th></tr></thead>
    <tbody></tbody>`;
  const tb = tbl.querySelector("tbody");
  for (const r of rules) {
    const st = (data.status || {})[r.id] || {};
    const active = !!st.active || !!(data.active || {})[r.id];
    const peer = (state.status?.peers || []).find((p) => p.id === r.peer_id);
    const rowCls = st.err ? "fw-error" : active ? "fw-active" : "fw-stopped";
    const tr = document.createElement("tr");
    tr.className = rowCls;
    tr.innerHTML = `
      <td class="fw-name">${esc(r.name || "（未命名）")}</td>
      <td><span class="p-badge" title="${r.mode === "L" ? "本机监听，经远端访问目标" : "远端监听，转回本机目标"}">-${esc(r.mode)}</span></td>
      <td class="mono">${esc(r.listen_host || "127.0.0.1")}:${r.listen_port}</td>
      <td class="mono">${esc(r.target_host)}:${r.target_port}</td>
      <td class="fw-state">${st.err ? "错误" : active ? "运行中" : "已停止"}</td>
      <td class="mono dim">${active && st.started_at ? fmtTime(st.started_at) : "—"}</td>
      <td class="fw-del"></td>`;
    tr.title = esc(peer?.name || shortID(r.peer_id));
    tr.querySelector(".fw-del").innerHTML = `<button class="btn small danger">删除</button>`;
    tr.querySelector(".fw-del button").addEventListener("click", async () => {
      try { await api("POST", "/api/fwd/remove", { id: r.id }); refreshFwd(); } catch (e) { toast(e.message, true); }
    });
    tb.appendChild(tr);
    if (st.err) {
      // Show the failure reason on a full-width row under the rule.
      const er = document.createElement("tr");
      er.className = "fw-err-row";
      er.innerHTML = `<td colspan="7">启动失败: ${esc(st.err)}</td>`;
      tb.appendChild(er);
    }
  }
  listEl.appendChild(tbl);
}

$("#fwd-mode").addEventListener("change", () => {
  const m = $("#fwd-mode").value;
  $("#fwd-hint").textContent = m === "L"
    ? "-L 例：本机 13389 → 远端 localhost:3389，访问本机 13389 即访问远端远程桌面。"
    : "-R 例：远端 9090 → 本机 localhost:8080，让远端设备访问本机服务。";
});

$("#fwd-add").addEventListener("click", async () => {
  const body = {
    peer_id: $("#fwd-peer").value,
    name: $("#fwd-name").value.trim(),
    mode: $("#fwd-mode").value,
    listen_host: "127.0.0.1",
    listen_port: parseInt($("#fwd-listen-port").value, 10),
    target_host: $("#fwd-target-host").value.trim() || "localhost",
    target_port: parseInt($("#fwd-target-port").value, 10),
  };
  if (!body.peer_id) { toast("请先选择设备", true); return; }
  if (!body.listen_port || !body.target_port) { toast("端口无效", true); return; }
  try { await api("POST", "/api/fwd/add", body); toast("已添加"); refreshFwd(); } catch (e) { toast(e.message, true); }
});

// ------------------------------------------------------------- settings ---

$("#set-save").addEventListener("click", async () => {
  try {
    await api("POST", "/api/name", { name: $("#set-name").value.trim() });
    toast("已保存");
    refreshStatus();
  } catch (e) { toast(e.message, true); }
});

$("#set-hub-save").addEventListener("click", async () => {
  const msg = $("#set-hub-msg");
  msg.textContent = "连接 hub 中…";
  try {
    await api("POST", "/api/hub", { addr: $("#set-hub").value.trim(), token: $("#set-hub-token").value.trim() });
    msg.textContent = "";
    toast("hub 已更新");
    refreshStatus();
  } catch (e) { msg.textContent = ""; toast(e.message, true); }
});
