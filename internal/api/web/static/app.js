"use strict";

// ---- tiny helpers ----
const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => [...document.querySelectorAll(sel)];
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

function human(b) {
  if (b == null) return "";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0, n = b;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return n.toFixed(n >= 100 || i === 0 ? 0 : 1) + " " + units[i];
}

let apiKey = localStorage.getItem("muxprune_api_key") || "";

async function api(path, opts = {}) {
  const headers = { ...(opts.headers || {}) };
  if (apiKey) headers["X-Api-Key"] = apiKey;
  if (opts.body && typeof opts.body !== "string") {
    opts.body = JSON.stringify(opts.body);
    headers["Content-Type"] = "application/json";
  }
  const r = await fetch("/api/v1" + path, { ...opts, headers });
  if (r.status === 401) {
    const key = prompt("API key required:");
    if (key) {
      apiKey = key;
      localStorage.setItem("muxprune_api_key", key);
      return api(path, opts);
    }
  }
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

let toastTimer;
function toast(msg, isError = false) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = isError ? "error" : "";
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), isError ? 6000 : 3000);
}

// ---- view switching ----
$$("nav button").forEach((b) =>
  b.addEventListener("click", () => {
    $$("nav button").forEach((x) => x.classList.toggle("active", x === b));
    ["files", "libraries", "jobs"].forEach((v) =>
      ($("#view-" + v).hidden = v !== b.dataset.view));
    refresh(b.dataset.view);
  }));

function currentView() {
  return $("nav button.active").dataset.view;
}

function refresh(view = currentView()) {
  loadStats();
  if (view === "files") loadFiles();
  if (view === "libraries") loadLibraries();
  if (view === "jobs") loadJobs();
}

async function loadStats() {
  try {
    const s = await api("/stats");
    $("#stats").innerHTML =
      `${s.files} files &middot; ${s.active_jobs} active jobs &middot; saved <b>${human(s.bytes_saved)}</b>`;
  } catch { /* server restarting */ }
}

// ---- libraries ----
let libraries = [];

async function loadLibraries() {
  libraries = await api("/libraries");
  const tbody = $("#libraries-table tbody");
  tbody.innerHTML = libraries.map((l) => `
    <tr data-id="${l.id}">
      <td>${esc(l.name)}</td><td class="muted">${esc(l.path)}</td><td>${esc(l.kind)}</td>
      <td>${esc(l.hardlink_policy)}</td>
      <td class="num">
        <button data-act="scan">Scan</button>
        <button data-act="edit">Edit</button>
        <button data-act="del" class="danger">Remove</button>
      </td>
    </tr>`).join("");
  $("#libraries-empty").hidden = libraries.length > 0;
  const sel = $("#filter-library");
  const cur = sel.value;
  sel.innerHTML = '<option value="">All libraries</option>' +
    libraries.map((l) => `<option value="${l.id}">${esc(l.name)}</option>`).join("");
  sel.value = cur;
}

$("#libraries-table").addEventListener("click", async (e) => {
  const btn = e.target.closest("button");
  if (!btn) return;
  const id = +btn.closest("tr").dataset.id;
  const lib = libraries.find((l) => l.id === id);
  try {
    if (btn.dataset.act === "scan") {
      await api(`/libraries/${id}/scan`, { method: "POST" });
      toast(`Scanning ${lib.name}...`);
    } else if (btn.dataset.act === "edit") {
      openLibraryDialog(lib);
    } else if (btn.dataset.act === "del") {
      if (!confirm(`Remove library "${lib.name}"? Files on disk are not touched.`)) return;
      await api(`/libraries/${id}`, { method: "DELETE" });
      loadLibraries();
    }
  } catch (err) { toast(err.message, true); }
});

function openLibraryDialog(lib) {
  const form = $("#dlg-library-form");
  form.reset();
  $("#dlg-library-title").textContent = lib ? "Edit library" : "Add library";
  form.dataset.id = lib ? lib.id : "";
  if (lib) for (const k of ["name", "path", "kind", "hardlink_policy"]) form[k].value = lib[k];
  $("#dlg-library").showModal();
}

$("#btn-add-library").addEventListener("click", () => openLibraryDialog(null));

// ---- folder browser ----
let currentBrowsePath = "/";
let parentBrowsePath = "";

async function loadBrowseDirectory(path) {
  try {
    const res = await api("/browse?path=" + encodeURIComponent(path));
    currentBrowsePath = res.current;
    parentBrowsePath = res.parent;

    $("#browse-path").textContent = res.current;
    $("#browse-back").disabled = !res.parent;

    const list = $("#browse-list");
    list.innerHTML = "";

    if (res.directories && res.directories.length > 0) {
      list.innerHTML = res.directories.map(dir => `
        <div class="browse-item" data-name="${esc(dir)}">
          <svg width="14" height="14" viewBox="0 0 16 16" fill="var(--accent)" style="display: block; margin-top: 1px;"><path d="M2 2a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V6a2 2 0 0 0-2-2H9L7.5 2H2z"/></svg>
          <span>${esc(dir)}</span>
        </div>
      `).join("");
    } else {
      list.innerHTML = '<div style="padding: 1.5rem; color: var(--faint); text-align: center; font-size: 13px;">Empty directory</div>';
    }
  } catch (err) {
    toast(err.message, true);
  }
}

$("#btn-browse-path").addEventListener("click", () => {
  const startingPath = $("#library-path").value.trim() || "/";
  loadBrowseDirectory(startingPath);
  $("#dlg-browse").showModal();
});

$("#btn-browse-cancel").addEventListener("click", () => {
  $("#dlg-browse").close();
});

$("#btn-browse-select").addEventListener("click", () => {
  $("#library-path").value = currentBrowsePath;
  $("#dlg-browse").close();
});

$("#browse-back").addEventListener("click", () => {
  if (parentBrowsePath) {
    loadBrowseDirectory(parentBrowsePath);
  }
});

$("#browse-list").addEventListener("click", (e) => {
  const item = e.target.closest(".browse-item");
  if (item) {
    const dirName = item.dataset.name;
    const separator = currentBrowsePath === "/" ? "" : "/";
    const nextPath = currentBrowsePath + separator + dirName;
    loadBrowseDirectory(nextPath);
  }
});

$("#btn-scan-all").addEventListener("click", async () => {
  try {
    const r = await api("/scan", { method: "POST" });
    toast(`Started ${r.started} scan(s)`);
  } catch (err) { toast(err.message, true); }
});

$("#dlg-library-form").addEventListener("submit", async (e) => {
  const form = e.target;
  if (e.submitter && e.submitter.value === "cancel") return;
  e.preventDefault();
  const body = Object.fromEntries(new FormData(form));
  try {
    if (form.dataset.id) await api(`/libraries/${form.dataset.id}`, { method: "PUT", body });
    else await api("/libraries", { method: "POST", body });
    $("#dlg-library").close();
    loadLibraries();
  } catch (err) { toast(err.message, true); }
});

// ---- files ----
const selection = new Set();
let filesCache = [];

function badges(summary, cls) {
  return (summary || "").split(" ").filter(Boolean)
    .map((x) => `<span class="badge ${cls}">${esc(x)}</span>`).join("");
}

function fileLabel(f) {
  if (f.kind === "tv" && f.episode) return `${f.series} ${f.episode}`;
  return f.title || f.path.split("/").pop();
}

async function loadFiles() {
  if (!libraries.length) await loadLibraries().catch(() => {});
  const params = new URLSearchParams();
  const lib = $("#filter-library").value;
  const q = $("#filter-q").value.trim();
  const kind = $("#filter-kind").value;
  const hardlinks = $("#filter-hardlinks").value;
  const subs = $("#filter-subs").value;
  const sort = $("#sort-by").value;
  const order = $("#sort-order").value;

  if (lib) params.set("library", lib);
  if (q) params.set("q", q);
  if (kind) params.set("kind", kind);
  if (hardlinks) params.set("hardlinks", hardlinks);
  if (subs) params.set("subs", subs);
  if (sort) params.set("sort", sort);
  if (order) params.set("order", order);
  params.set("limit", "500");

  const data = await api("/files?" + params);
  filesCache = data.files;
  const tbody = $("#files-table tbody");
  tbody.innerHTML = data.files.map((f) => `
    <tr data-id="${f.id}">
      <td class="col-check"><input type="checkbox" data-id="${f.id}" ${selection.has(f.id) ? "checked" : ""}></td>
      <td>
        ${esc(fileLabel(f))}${f.nlink > 1 ? ' <span class="badge warn" title="Hardlinked: remux would break seeding">&#128279; hardlink</span>' : ""}
        <div class="sub">${esc(f.path)}</div>
      </td>
      <td><span class="badge">${esc(f.video_codec)}</span></td>
      <td>${badges(f.audio_summary, "audio")}</td>
      <td>${badges(f.sub_summary, "subtitle")}</td>
      <td>${badges(f.sidecar_summary, "sidecar")}</td>
      <td class="num">${human(f.size)}</td>
      <td class="num"><button data-act="open">Edit</button></td>
    </tr>`).join("");
  $("#files-empty").hidden = data.files.length > 0;
  updateSelectionUI();
}

let qTimer;
$("#filter-q").addEventListener("input", () => { clearTimeout(qTimer); qTimer = setTimeout(loadFiles, 300); });
$("#filter-library").addEventListener("change", loadFiles);
$("#filter-kind").addEventListener("change", loadFiles);
$("#filter-hardlinks").addEventListener("change", loadFiles);
$("#filter-subs").addEventListener("change", loadFiles);
$("#sort-by").addEventListener("change", loadFiles);
$("#sort-order").addEventListener("change", loadFiles);

$("#files-table").addEventListener("click", (e) => {
  const check = e.target.closest('input[type="checkbox"]');
  if (check && check.id !== "check-all") {
    check.checked ? selection.add(+check.dataset.id) : selection.delete(+check.dataset.id);
    updateSelectionUI();
    return;
  }
  const row = e.target.closest("tr[data-id]");
  if (row && (e.target.closest("button") || !e.target.closest("input")))
    openFileDialog(+row.dataset.id);
});

$("#check-all").addEventListener("change", (e) => {
  filesCache.forEach((f) => (e.target.checked ? selection.add(f.id) : selection.delete(f.id)));
  $$('#files-table tbody input[type="checkbox"]').forEach((c) => (c.checked = e.target.checked));
  updateSelectionUI();
});

function updateSelectionUI() {
  $("#selection-info").textContent = selection.size ? `${selection.size} selected` : "";
  $("#btn-batch").disabled = selection.size === 0;
}

// ---- file dialog ----
let dlgFile = null;

async function openFileDialog(id) {
  try {
    dlgFile = await api(`/files/${id}`);
  } catch (err) { return toast(err.message, true); }
  $("#dlg-file-title").textContent = fileLabel(dlgFile);
  $("#dlg-file-path").textContent =
    `${dlgFile.path} (${human(dlgFile.size)})` + (dlgFile.nlink > 1 ? ` — ${dlgFile.nlink} hardlinks` : "");
  
  const isMKV = dlgFile.format && dlgFile.format.includes("matroska");
  
  // Show/hide MKV specific sections
  $("#dlg-save-metadata").style.display = isMKV ? "inline-block" : "none";
  $("#dlg-merge-section").style.display = isMKV ? "block" : "none";
  $("#dlg-save-order").style.display = "none"; // hidden until a reorder happens
  $("#dlg-merge-path").value = "";

  const audio = dlgFile.streams.filter((s) => s.type === "audio");
  
  function renderStreams() {
    $("#dlg-streams").innerHTML = dlgFile.streams.map((s, idx) => {
      const removable = s.type === "audio" || s.type === "subtitle";
      const isReorderable = isMKV && s.mkv_id >= 0;
      
      const reorderControls = isReorderable ? `
        <span style="display: inline-flex; gap: 2px;">
          <button type="button" class="btn-reorder" data-action="up" data-idx="${idx}" style="padding: 2px 4px; font-size: 10px; line-height: 1;" ${idx === 0 ? "disabled" : ""}>&uarr;</button>
          <button type="button" class="btn-reorder" data-action="down" data-idx="${idx}" style="padding: 2px 4px; font-size: 10px; line-height: 1;" ${idx === dlgFile.streams.length - 1 ? "disabled" : ""}>&darr;</button>
        </span>
      ` : `<span style="width: 32px; display: inline-block;"></span>`;

      const editControls = isMKV && s.mkv_id >= 0 ? `
        <span style="display: inline-flex; align-items: center; gap: 4px; margin-left: auto;">
          <input type="text" class="edit-lang" data-idx="${s.index}" value="${esc(s.lang || "")}" placeholder="lang" style="width: 45px; padding: 2px; font-size: 11px; height: 22px;">
          <input type="text" class="edit-title" data-idx="${s.index}" value="${esc(s.title || "")}" placeholder="title" style="width: 100px; padding: 2px; font-size: 11px; height: 22px;">
          <label style="margin: 0; display: inline-flex; align-items: center; gap: 2px; font-size: 11px; user-select: none;">
            <input type="checkbox" class="edit-default" data-idx="${s.index}" ${s.default ? "checked" : ""}> def
          </label>
          <label style="margin: 0; display: inline-flex; align-items: center; gap: 2px; font-size: 11px; user-select: none;">
            <input type="checkbox" class="edit-forced" data-idx="${s.index}" ${s.forced ? "checked" : ""}> forced
          </label>
        </span>
      ` : "";

      const detail = [s.codec, s.channel_layout].filter(Boolean).join(" · ");
      
      return `<div class="stream-row ${removable ? "" : "locked"}" style="display: flex; align-items: center; gap: 6px;">
        <input type="checkbox" data-idx="${s.index}" data-type="${s.type}" ${removable ? "" : "disabled"}>
        ${reorderControls}
        <span class="tag" style="min-width: 4.8rem; font-size: 11.5px;">#${s.index} ${esc(s.type)}</span>
        <span style="font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 180px;" title="${esc(detail)}">${esc(detail)}</span>
        ${editControls}
      </div>`;
    }).join("");
    
    // Add event listeners to the reorder buttons
    $$(".btn-reorder").forEach((btn) => {
      btn.addEventListener("click", (e) => {
        e.stopPropagation();
        const action = btn.dataset.action;
        const currentIdx = parseInt(btn.dataset.idx);
        const targetIdx = action === "up" ? currentIdx - 1 : currentIdx + 1;
        
        // Swap streams in dlgFile.streams
        const temp = dlgFile.streams[currentIdx];
        dlgFile.streams[currentIdx] = dlgFile.streams[targetIdx];
        dlgFile.streams[targetIdx] = temp;
        
        renderStreams();
        $("#dlg-save-order").style.display = "inline-block";
      });
    });

    // Synchronize inputs back to streams
    $$(".edit-lang").forEach((input) => {
      input.addEventListener("input", (e) => {
        const idx = parseInt(input.dataset.idx);
        const stream = dlgFile.streams.find((s) => s.index === idx);
        if (stream) stream.lang = e.target.value;
      });
    });
    $$(".edit-title").forEach((input) => {
      input.addEventListener("input", (e) => {
        const idx = parseInt(input.dataset.idx);
        const stream = dlgFile.streams.find((s) => s.index === idx);
        if (stream) stream.title = e.target.value;
      });
    });
    $$(".edit-default").forEach((input) => {
      input.addEventListener("change", (e) => {
        const idx = parseInt(input.dataset.idx);
        const stream = dlgFile.streams.find((s) => s.index === idx);
        if (stream) stream.default = e.target.checked;
      });
    });
    $$(".edit-forced").forEach((input) => {
      input.addEventListener("change", (e) => {
        const idx = parseInt(input.dataset.idx);
        const stream = dlgFile.streams.find((s) => s.index === idx);
        if (stream) stream.forced = e.target.checked;
      });
    });
  }

  renderStreams();

  $("#dlg-sidecars").innerHTML = (dlgFile.sidecars || []).map((sc) => `
    <div class="stream-row">
      <input type="checkbox" data-sidecar="${sc.id}">
      <span class="tag">sidecar</span>
      <span>${esc(sc.name)} <span class="muted">(${human(sc.size)})</span></span>
    </div>`).join("") || '<p class="muted small">None found next to this file.</p>';
  $("#dlg-allow-hardlink").checked = false;
  $("#dlg-preview").hidden = true;
  if (audio.length === 0) toast("No audio streams found in this file", true);
  $("#dlg-file").showModal();
}

function fileDialogRequest(dry) {
  const remove_audio = [], remove_subs = [], delete_sidecars = [];
  $$('#dlg-streams input:checked').forEach((c) =>
    (c.dataset.type === "audio" ? remove_audio : remove_subs).push(+c.dataset.idx));
  $$('#dlg-sidecars input:checked').forEach((c) => delete_sidecars.push(+c.dataset.sidecar));
  return {
    remove_audio, remove_subs, delete_sidecars,
    allow_hardlink: $("#dlg-allow-hardlink").checked, dry_run: dry,
  };
}

$("#dlg-dryrun").addEventListener("click", async () => {
  const req = fileDialogRequest(true);
  if (!req.remove_audio.length && !req.remove_subs.length && !req.delete_sidecars.length)
    return toast("Select something to remove first");
  try {
    const r = await api(`/files/${dlgFile.id}/jobs`, { method: "POST", body: req });
    const lines = [];
    if (r.remux) lines.push(`remux via ${r.remux.tool}, est. savings ${human(r.remux.bytes_saved)}`,
      r.remux.command);
    (r.delete_sidecars || []).forEach((p) => lines.push("delete " + p));
    const box = $("#dlg-preview");
    box.textContent = lines.join("\n") || "nothing to do";
    box.hidden = false;
  } catch (err) { toast(err.message, true); }
});

$("#dlg-submit").addEventListener("click", async () => {
  const req = fileDialogRequest(false);
  if (!req.remove_audio.length && !req.remove_subs.length && !req.delete_sidecars.length)
    return toast("Select something to remove first");
  try {
    const r = await api(`/files/${dlgFile.id}/jobs`, { method: "POST", body: req });
    toast(`Queued ${r.jobs.length} job(s)`);
    $("#dlg-file").close();
  } catch (err) { toast(err.message, true); }
});

$("#dlg-save-order").addEventListener("click", async () => {
  const track_order = dlgFile.streams
    .filter((s) => s.mkv_id >= 0)
    .map((s) => s.index);
  try {
    await api(`/files/${dlgFile.id}/reorder`, {
      method: "POST",
      body: { track_order }
    });
    toast("Queued track reordering job");
    $("#dlg-file").close();
  } catch (err) { toast(err.message, true); }
});

$("#dlg-save-metadata").addEventListener("click", async () => {
  const edits = dlgFile.streams
    .filter((s) => s.mkv_id >= 0)
    .map((s) => ({
      track_index: s.index,
      language: s.lang || "",
      title: s.title || "",
      default: s.default,
      forced: s.forced,
    }));
  try {
    await api(`/files/${dlgFile.id}/metadata`, {
      method: "POST",
      body: { edits }
    });
    toast("Queued header edit job");
    $("#dlg-file").close();
  } catch (err) { toast(err.message, true); }
});

$("#dlg-merge-btn").addEventListener("click", async () => {
  const path = $("#dlg-merge-path").value.trim();
  if (!path) return toast("Enter external file path first");
  try {
    await api(`/files/${dlgFile.id}/merge`, {
      method: "POST",
      body: { external_files: [path] }
    });
    toast("Queued track merge job");
    $("#dlg-file").close();
  } catch (err) { toast(err.message, true); }
});

// ---- batch dialog ----
$("#btn-batch").addEventListener("click", () => {
  $("#dlg-batch-form").reset();
  $("#batch-count").textContent = selection.size;
  $("#batch-preview").hidden = true;
  $("#dlg-batch").showModal();
});

function batchRequest(dry) {
  const f = $("#dlg-batch-form");
  const langs = (name) => f[name].value.split(",").map((s) => s.trim()).filter(Boolean);
  return {
    file_ids: [...selection],
    keep_audio_langs: langs("keep_audio_langs"),
    remove_audio_langs: langs("remove_audio_langs"),
    remove_sub_langs: langs("remove_sub_langs"),
    remove_all_subs: f.remove_all_subs.checked,
    delete_sidecar_langs: langs("delete_sidecar_langs"),
    delete_all_sidecars: f.delete_all_sidecars.checked,
    allow_hardlink: f.allow_hardlink.checked,
    dry_run: dry,
  };
}

$("#batch-dryrun").addEventListener("click", async () => {
  try {
    const r = await api("/batch", { method: "POST", body: batchRequest(true) });
    const lines = r.results.map((x) => {
      const acts = [];
      if (x.remove_audio?.length) acts.push(`-${x.remove_audio.length} audio`);
      if (x.remove_subs?.length) acts.push(`-${x.remove_subs.length} subs`);
      if (x.sidecar_names?.length) acts.push("del " + x.sidecar_names.join(", "));
      const note = x.notes?.length ? `  [${x.notes.join("; ")}]` : "";
      return `${x.path.split("/").pop()}: ${acts.join(", ") || "no change"}${note}`;
    });
    const box = $("#batch-preview");
    box.textContent = lines.join("\n");
    box.hidden = false;
  } catch (err) { toast(err.message, true); }
});

$("#batch-submit").addEventListener("click", async () => {
  try {
    const r = await api("/batch", { method: "POST", body: batchRequest(false) });
    toast(`Queued ${r.total_jobs} job(s)`);
    $("#dlg-batch").close();
    selection.clear();
    updateSelectionUI();
  } catch (err) { toast(err.message, true); }
});

// ---- jobs ----
async function loadJobs() {
  const jobs = await api("/jobs?limit=200");
  const tbody = $("#jobs-table tbody");
  tbody.innerHTML = jobs.map((j) => `
    <tr>
      <td>${j.id}</td><td>${esc(j.type)}</td>
      <td class="sub">${esc(j.file_path)}</td>
      <td><span class="status ${esc(j.status)}">${esc(j.status)}</span></td>
      <td class="num">${j.bytes_saved ? human(j.bytes_saved) : ""}</td>
      <td class="sub">${esc(j.log)}</td>
    </tr>`).join("");
  $("#jobs-empty").hidden = jobs.length > 0;
}
$("#jobs-table").style.cursor = "default";

// ---- live events ----
function connectEvents() {
  const url = "/api/v1/events" + (apiKey ? "?apikey=" + encodeURIComponent(apiKey) : "");
  const es = new EventSource(url);
  es.addEventListener("scan", (e) => {
    const d = JSON.parse(e.data);
    const box = $("#scan-progress");
    if (d.phase === "done" || d.phase === "error") {
      box.hidden = true;
      if (d.phase === "error") toast("Scan failed: " + d.error, true);
      else if (currentView() === "files") loadFiles();
      loadStats();
    } else {
      box.hidden = false;
      box.textContent = `Scanning library ${d.library_id}: ${d.done}/${d.total} ${d.path || ""}`;
    }
  });
  es.addEventListener("job", (e) => {
    const d = JSON.parse(e.data);
    if (currentView() === "jobs") loadJobs();
    if (d.status && d.status !== "running" && d.status !== "queued") {
      toast(`Job #${d.id} ${d.status}${d.bytes_saved ? " · saved " + human(d.bytes_saved) : ""}`,
        d.status === "failed");
      if (currentView() === "files") loadFiles();
      loadStats();
    }
  });
  es.onerror = () => { es.close(); setTimeout(connectEvents, 5000); };
}

// ---- boot ----
loadLibraries().then(loadFiles).catch((e) => toast(e.message, true));
loadStats();
connectEvents();
