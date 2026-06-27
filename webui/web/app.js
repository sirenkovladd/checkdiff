// checkdiff web UI client. Talks to the daemon's JSON API using
// the Bearer token stored in localStorage. The token persists
// across page reloads so the user signs in once per browser;
// the "Sign out" button clears it.

const TOKEN_KEY = "checkdiff.token";

const $ = (sel) => document.querySelector(sel);

function authHeaders() {
  const t = localStorage.getItem(TOKEN_KEY) || "";
  return { "Authorization": "Bearer " + t, "Content-Type": "application/json" };
}

async function api(path, options = {}) {
  options.headers = { ...authHeaders(), ...(options.headers || {}) };
  const resp = await fetch(path, options);
  if (resp.status === 401) {
    localStorage.removeItem(TOKEN_KEY);
    showLogin("Token rejected. Please sign in again.");
    return null;
  }
  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(`${resp.status} ${resp.statusText}: ${text}`);
  }
  if (resp.status === 204) return null;
  return resp.json();
}

function showLogin(error) {
  $("#login-section").hidden = false;
  $("#main-section").hidden = true;
  if (error) {
    $("#login-error").textContent = error;
    $("#login-error").hidden = false;
  } else {
    $("#login-error").hidden = true;
  }
}

function showMain() {
  $("#login-section").hidden = true;
  $("#main-section").hidden = false;
  loadAll();
}

async function connect() {
  const t = $("#token-input").value.trim();
  if (!t) {
    showLogin("Token cannot be empty");
    return;
  }
  localStorage.setItem(TOKEN_KEY, t);
  // Verify the token by hitting /api/state. If it 401s, the api()
  // helper will clear localStorage and show the login form.
  try {
    const state = await api("/api/state");
    if (state !== null) showMain();
  } catch (e) {
    showLogin("Sign-in failed: " + e.message);
  }
}

function logout() {
  localStorage.removeItem(TOKEN_KEY);
  $("#token-input").value = "";
  showLogin();
}

async function loadAll() {
  const [sources, state, config] = await Promise.all([
    api("/api/sources"),
    api("/api/state"),
    api("/api/config"),
  ]);
  renderSources(sources || [], state || {});
  // Cache the current settings for the Settings dialog. The web
  // token comes back masked ("****") so we don't overwrite the
  // user's input on every reload.
  $("#settings-server").value = config?.ntfy?.server || "";
  $("#settings-topic").value = config?.ntfy?.topic || "";
  $("#settings-interval").value = config?.check?.interval || "";
  $("#settings-listen").value = config?.web?.listen || "";
}

function renderSources(sources, state) {
  const tbody = $("#sources-tbody");
  tbody.innerHTML = "";
  for (const src of sources) {
    const s = state[src.id] || {};
    const tr = document.createElement("tr");
    tr.appendChild(td(src.id));
    tr.appendChild(td(src.name));
    tr.appendChild(td(src.type));
    tr.appendChild(td(src.url || ""));
    tr.appendChild(td(src.check_interval || ""));
    tr.appendChild(td(src.enabled !== false ? "yes" : "no"));
    tr.appendChild(td(s.last_run ? new Date(s.last_run).toLocaleString() : "—"));
    tr.appendChild(td(s.next_run ? new Date(s.next_run).toLocaleString() : "—"));
    tr.appendChild(td(s.items_count != null ? String(s.items_count) : "—"));
    // items_hash is "sha256:<64 hex chars>". The first 8 hex
    // chars after the prefix are plenty for a visual fingerprint;
    // the full hash is in the API response if the user needs it.
    const fullHash = s.items_hash || "";
    const shortHash = fullHash.startsWith("sha256:") ? fullHash.slice(7, 15) : (fullHash ? fullHash.slice(0, 8) : "—");
    tr.appendChild(td(shortHash, { title: fullHash || "no hash yet" }));
    tr.appendChild(td(`+${s.last_added || 0} / -${s.last_removed || 0}`));
    const actions = document.createElement("td");
    const runBtn = btn("Run now", () => runNow(src.id));
    const editBtn = btn("Edit", () => openSourceDialog(src));
    const delBtn = btn("Delete", () => deleteSource(src.id));
    actions.append(runBtn, editBtn, delBtn);
    tr.appendChild(actions);
    tbody.appendChild(tr);
    if (s.last_error) {
      const errRow = document.createElement("tr");
      const errCell = document.createElement("td");
      errCell.colSpan = 12;
      errCell.className = "error";
      errCell.textContent = "Error: " + s.last_error;
      errRow.appendChild(errCell);
      tbody.appendChild(errRow);
    }
  }
}

function td(text, attrs) {
  const e = document.createElement("td");
  e.textContent = text;
  if (attrs) {
    for (const k of Object.keys(attrs)) {
      e.setAttribute(k, attrs[k]);
    }
  }
  return e;
}

function btn(label, onclick) {
  const e = document.createElement("button");
  e.className = "action";
  e.textContent = label;
  e.onclick = onclick;
  return e;
}

async function runNow(id) {
  await api(`/api/sources/${encodeURIComponent(id)}/run`, { method: "POST" });
  setTimeout(loadAll, 500); // give the goroutine a moment to update state
}

async function deleteSource(id) {
  if (!confirm(`Delete source ${id}?`)) return;
  await api(`/api/sources/${encodeURIComponent(id)}`, { method: "DELETE" });
  loadAll();
}

function openSourceDialog(src) {
  const dlg = $("#source-dialog");
  const isEdit = !!src;
  $("#source-dialog-title").textContent = isEdit ? "Edit source" : "Add source";
  $("#source-type").value = src?.type || "json";
  $("#source-id").value = src?.id || "";
  $("#source-id").readOnly = isEdit; // ID is the lookup key; don't let it change.
  $("#source-name").value = src?.name || "";
  $("#source-url").value = src?.url || "";
  $("#source-interval").value = src?.check_interval || "";
  $("#source-enabled").checked = src?.enabled !== false;
  renderTypeFields(src);
  dlg.showModal();
}

function renderTypeFields(src) {
  const type = $("#source-type").value;
  const container = $("#source-type-fields");
  container.innerHTML = "";
  const fields = {
    github_file: ["owner", "repo", "ref", "path"],
    html: ["selector"],
    json: ["items_path", "id_field", "title_field", "link_field", "link"],
  }[type] || [];
  for (const f of fields) {
    const label = document.createElement("label");
    label.textContent = f;
    const input = document.createElement("input");
    input.name = f;
    input.id = "source-" + f;
    input.value = src?.[f] || "";
    label.appendChild(input);
    container.appendChild(label);
  }
}

$("#source-type").addEventListener("change", () => renderTypeFields(null));

$("#source-form").addEventListener("submit", async (e) => {
  if (e.submitter?.value !== "default") return; // Cancel button
  e.preventDefault();
  const data = collectSourceForm();
  const id = data.id;
  const isEdit = $("#source-id").readOnly;
  const resp = isEdit
    ? await api(`/api/sources/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify(data) })
    : await api(`/api/sources`, { method: "POST", body: JSON.stringify(data) });
  if (resp !== null) {
    $("#source-dialog").close();
    loadAll();
  }
});

function collectSourceForm() {
  const get = (id) => $(id).value;
  const data = {
    type: get("#source-type"),
    id: get("#source-id").trim(),
    name: get("#source-name").trim(),
    url: get("#source-url").trim(),
    check_interval: get("#source-interval").trim(),
    enabled: $("#source-enabled").checked,
  };
  // Type-specific fields.
  const type = data.type;
  for (const f of ["owner", "repo", "ref", "path", "selector", "items_path", "id_field", "title_field", "link_field", "link"]) {
    const el = $("#source-" + f);
    if (el && el.value) data[f] = el.value;
  }
  return data;
}

$("#settings-btn").addEventListener("click", () => {
  // The token field is intentionally left blank on open — typing
  // a new value rotates the token, leaving it blank keeps the
  // current one (the server sees the absence of the field and
  // leaves the existing token alone). The masked value from
  // /api/config is "****" so we can't pre-fill.
  $("#settings-token").value = "";
  $("#settings-rotated").hidden = true;
  $("#settings-error").hidden = true;
  $("#settings-dialog").showModal();
});

$("#settings-rotate").addEventListener("click", async () => {
  // Confirm before rotating — the old token is invalidated
  // immediately and any other browser signed in will be
  // signed out on their next request.
  if (!confirm("Rotate the web token? Other browsers signed in with the current token will be signed out.")) {
    return;
  }
  try {
    const resp = await api("/api/rotate-token", { method: "POST" });
    if (resp && resp.token) {
      // Persist in this browser immediately so the next API
      // call doesn't 401.
      localStorage.setItem(TOKEN_KEY, resp.token);
      $("#settings-token").value = "";
      const el = $("#settings-rotated");
      el.textContent = "New token: " + resp.token + " (copy it now — this is the only time it will be shown)";
      el.hidden = false;
    }
  } catch (err) {
    const el = $("#settings-error");
    el.textContent = err.message;
    el.hidden = false;
  }
});

$("#settings-form").addEventListener("submit", async (e) => {
  if (e.submitter?.value !== "default") return;
  e.preventDefault();
  const body = {
    ntfy: { server: $("#settings-server").value, topic: $("#settings-topic").value },
    check: { interval: $("#settings-interval").value },
    web:   { listen: $("#settings-listen").value },
  };
  // Only include the token in the body if the user typed something.
  // An empty input means "don't change the token".
  if ($("#settings-token").value) {
    body.web.token = $("#settings-token").value;
  }
  try {
    await api("/api/settings", { method: "PUT", body: JSON.stringify(body) });
    $("#settings-dialog").close();
    // The token may have changed; the next request will be 401 if
    // the new token is required, and the api() helper will clear
    // localStorage and re-prompt. We re-store in case the user
    // rotated to a new token and the server is now using it.
    if (body.web.token) {
      localStorage.setItem(TOKEN_KEY, body.web.token);
    }
    loadAll();
  } catch (err) {
    $("#settings-error").textContent = err.message;
    $("#settings-error").hidden = false;
  }
});

$("#connect-btn").addEventListener("click", connect);
$("#token-input").addEventListener("keydown", (e) => { if (e.key === "Enter") connect(); });
$("#refresh").addEventListener("click", loadAll);
$("#logout-btn").addEventListener("click", logout);

// On load: if the URL has ?token=..., capture it into
// localStorage so the user doesn't have to retype it. The
// server-side auth middleware already accepts the query token
// for this request; the JS just needs to remember it for
// subsequent ones. Then strip the token from the URL so it
// doesn't leak into browser history, referer headers, share
// links, or the address bar.
const initialToken = new URLSearchParams(location.search).get("token");
if (initialToken) {
  localStorage.setItem(TOKEN_KEY, initialToken);
  history.replaceState(null, "", location.pathname + location.hash);
}

// On load: try the stored token; fall back to the login form.
if (localStorage.getItem(TOKEN_KEY)) {
  api("/api/state").then((s) => { if (s !== null) showMain(); });
} else {
  showLogin();
}

// Auto-refresh the main view every 5 seconds while signed in.
setInterval(() => {
  if (!$("#main-section").hidden) loadAll();
}, 5000);
