"use strict";
// Buttons call the JSON API with the session's CSRF token; the groups are
// re-rendered by the server every 10 seconds.
(function () {
  const csrf = (document.querySelector('meta[name="csrf-token"]') || {}).content || "";
  const groups = document.getElementById("groups");
  const message = document.getElementById("message");
  let busy = false;

  function localize(root) {
    const now = new Date();
    root.querySelectorAll("time[datetime]").forEach(function (el) {
      const d = new Date(el.getAttribute("datetime"));
      if (isNaN(d)) return;
      const opts = { hour: "2-digit", minute: "2-digit" };
      if (d.toDateString() !== now.toDateString()) opts.weekday = "short";
      el.textContent = d.toLocaleString(undefined, opts);
      el.title = d.toString();
    });
  }

  function show(text, isError) {
    message.textContent = text;
    message.hidden = !text;
    message.classList.toggle("error", !!isError);
  }

  async function call(method, path, body) {
    const opts = { method: method, headers: { "X-CSRF-Token": csrf }, credentials: "same-origin" };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    const resp = await fetch(path, opts);
    if (resp.status === 401) {
      location.href = "/auth/login?return=/";
      return;
    }
    if (!resp.ok) {
      let msg = resp.status + " " + resp.statusText;
      try { msg = (await resp.json()).error || msg; } catch (e) { /* not JSON */ }
      throw new Error(msg);
    }
  }

  function request(el, group) {
    const base = "/api/v1/groups/" + encodeURIComponent(group);
    switch (el.dataset.action) {
      case "wake": return ["PUT", base + "/override", { state: "awake", ttl: el.dataset.ttl }];
      case "sleep": return ["PUT", base + "/override", { state: "asleep" }];
      case "default": return ["DELETE", base + "/override"];
      case "unhold": return ["DELETE", base + "/hold"];
      case "release": return ["DELETE", base + "/leases/" + encodeURIComponent(el.dataset.owner)];
      case "clear-conflict": return ["DELETE", base + "/conflict"];
      case "hold": {
        const f = new FormData(el);
        return ["PUT", base + "/hold", { reason: f.get("reason"), ttl: f.get("ttl") }];
      }
    }
    return null;
  }

  async function run(el) {
    const row = el.closest("[data-group]");
    const req = row && request(el, row.dataset.group);
    if (!req) return;
    busy = true;
    show("");
    row.classList.add("pending");
    try {
      await call(req[0], req[1], req[2]);
      await refresh(true);
    } catch (e) {
      show(row.dataset.group + ": " + e.message, true);
    } finally {
      busy = false;
      row.classList.remove("pending");
    }
  }

  groups.addEventListener("click", function (e) {
    const b = e.target.closest("button[data-action]");
    if (b) { e.preventDefault(); run(b); }
  });
  groups.addEventListener("submit", function (e) {
    const f = e.target.closest("form[data-action]");
    if (f) { e.preventDefault(); run(f); }
  });

  async function refresh(force) {
    const active = document.activeElement;
    if (!force && (busy || (groups.contains(active) && active.matches("input, select")))) return;
    // Keep hold reasons that are being typed.
    const drafts = {};
    groups.querySelectorAll("[data-group] input[name=reason]").forEach(function (i) {
      if (i.value) drafts[i.closest("[data-group]").dataset.group] = i.value;
    });
    try {
      const resp = await fetch("/ui/groups", { credentials: "same-origin" });
      if (resp.status === 401) { location.href = "/auth/login?return=/"; return; }
      if (!resp.ok) return;
      groups.innerHTML = await resp.text();
    } catch (e) {
      return;
    }
    groups.querySelectorAll("[data-group]").forEach(function (row) {
      const input = row.querySelector("input[name=reason]");
      if (input && drafts[row.dataset.group]) input.value = drafts[row.dataset.group];
    });
    localize(groups);
    document.getElementById("updated").textContent = new Date().toLocaleTimeString();
  }

  localize(document);
  setInterval(refresh, 10000);
})();
