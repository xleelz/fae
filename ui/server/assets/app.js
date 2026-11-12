"use strict";
// Fae UI: read-only views over the daemon's Status, Board, History and Alerts
// RPCs. Every value is untrusted text and is written with textContent only.
(function () {
  const $ = (id) => document.getElementById(id);
  let boardCursor = "";
  let historyTask = "";
  let historyCursor = 0;

  async function get(path) {
    const res = await fetch(path, { credentials: "same-origin", cache: "no-store" });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || res.statusText);
    return body;
  }
  function text(v) {
    if (v === null || v === undefined) return "";
    return typeof v === "object" ? JSON.stringify(v) : String(v);
  }
  function cell(tr, v, cls) {
    const td = document.createElement("td");
    td.textContent = text(v);
    if (cls) td.className = cls;
    tr.appendChild(td);
  }
  function pairs(dl, rows) {
    dl.replaceChildren();
    for (const [k, v] of rows) {
      const dt = document.createElement("dt");
      dt.textContent = k;
      const dd = document.createElement("dd");
      dd.textContent = text(v);
      dl.append(dt, dd);
    }
  }
  function banner(msg, cls) {
    const b = $("banner");
    b.textContent = msg;
    b.className = cls || "muted";
  }

  async function status() {
    const s = await get("/api/status");
    $("instance").textContent = text(s.instance);
    const snap = s.snapshot || {};
    const drive = snap.drive || {};
    const ui = s.ui || {};
    pairs($("status"), [
      ["pid", s.pid], ["uptime (s)", Math.round(s.uptime_seconds || 0)],
      ["version", (s.version || {}).version + " " + (s.version || {}).commit],
      ["adapter", s.adapter], ["plane", (s.plane || {}).endpoint],
      ["drive", (drive.alive ? "running" : "stopped") + ", rounds " + text(drive.rounds) + ", parked " + text(drive.parked)],
      ["maintenance", snap.maintenance], ["draining", snap.draining], ["shutting down", s.shutting_down],
      ["UI", text(ui.health) + ", restarts " + text(ui.restarts) + (ui.writes_enabled ? ", writes enabled" : ", read-only")],
      ["snapshot age (ms)", snap.age_ms],
    ]);
    pairs($("counts"), Object.entries(snap.counts || {}).sort());
    banner(s.shutting_down ? "Daemon is shutting down" : "Updated " + new Date().toLocaleTimeString(), s.shutting_down ? "warn" : "muted");
  }

  async function board(append) {
    const page = await get("/api/board?limit=100&after=" + encodeURIComponent(append ? boardCursor : ""));
    const body = $("board");
    if (!append) body.replaceChildren();
    for (const t of page.items || []) {
      const tr = document.createElement("tr");
      tr.className = "link";
      tr.addEventListener("click", () => history(t.id, false));
      cell(tr, t.id); cell(tr, t.state, t.state === "Escalated" ? "bad" : "");
      cell(tr, t.priority); cell(tr, t.workflow_id + " r" + t.workflow_revision);
      cell(tr, t.mode); cell(tr, t.in_flight ? "yes" : ""); cell(tr, t.escalation_kind || t.reason);
      body.appendChild(tr);
    }
    boardCursor = page.next_cursor || "";
    $("board-more").hidden = !boardCursor;
  }

  async function history(task, append) {
    if (!append) { historyTask = task; historyCursor = 0; }
    const page = await get("/api/history?limit=100&task=" + encodeURIComponent(historyTask) + "&after=" + historyCursor);
    $("history-section").hidden = false;
    $("history-task").textContent = historyTask;
    const events = $("history"), artifacts = $("artifacts");
    if (!append) { events.replaceChildren(); artifacts.replaceChildren(); }
    for (const e of page.events || []) {
      const tr = document.createElement("tr");
      cell(tr, e.sequence); cell(tr, e.timestamp); cell(tr, e.kind); cell(tr, e.payload);
      events.appendChild(tr);
    }
    for (const a of page.artifact_references || []) {
      const tr = document.createElement("tr");
      cell(tr, a.artifact_id); cell(tr, a.stage); cell(tr, a.role); cell(tr, a.produced_at);
      artifacts.appendChild(tr);
    }
    historyCursor = page.next_cursor || 0;
    $("history-more").hidden = !historyCursor;
  }

  async function alerts() {
    const page = await get("/api/alerts?limit=100");
    const body = $("alerts");
    body.replaceChildren();
    for (const a of page.alerts || []) {
      const tr = document.createElement("tr");
      cell(tr, a.id); cell(tr, a.severity, a.severity === "error" ? "bad" : "warn"); cell(tr, a.source);
      cell(tr, a.task); cell(tr, a.created_at); cell(tr, a.acknowledged_at); cell(tr, a.detail);
      body.appendChild(tr);
    }
  }

  async function acknowledge(ev) {
    ev.preventDefault();
    const result = $("ack-result");
    try {
      const res = await fetch("/api/alerts/ack", {
        method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id: Number($("ack-id").value), authorizing_message: $("ack-message").value }),
      });
      const body = await res.json().catch(() => ({}));
      result.textContent = res.ok ? "acknowledged" : text(body.error);
      await alerts();
    } catch (e) {
      result.textContent = e.message;
    }
  }

  async function refresh() {
    try {
      await Promise.all([status(), board(false), alerts()]);
      if (historyTask) await history(historyTask, false);
    } catch (e) {
      banner("Daemon unavailable: " + e.message, "bad");
    }
  }

  document.addEventListener("DOMContentLoaded", async () => {
    $("board-more").addEventListener("click", () => board(true).catch((e) => banner(e.message, "bad")));
    $("history-more").addEventListener("click", () => history(historyTask, true).catch((e) => banner(e.message, "bad")));
    $("ack").addEventListener("submit", acknowledge);
    try {
      const session = await get("/api/session");
      $("ack").hidden = !session.writes;
    } catch (e) {
      banner("Not signed in: open the URL printed by fae ui token", "bad");
      return;
    }
    await refresh();
    setInterval(refresh, 5000);
  });
})();
