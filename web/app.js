(() => {
  "use strict";

  const state = {
    token: null,
    sessionId: null,
    filename: "",
    metadata: null,
    busy: false,
  };

  const $ = (id) => document.getElementById(id);

  function formatSize(n) {
    if (typeof n !== "number" || !isFinite(n)) return String(n);
    if (n < 1024) return `${n} B`;
    const units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
    let v = n / 1024;
    let i = 0;
    while (v >= 1024 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return `${v.toFixed(2)} ${units[i]}`;
  }

  function setBusy(busy) {
    state.busy = busy;
    document.querySelectorAll("button").forEach((b) => {
      if (!b.dataset.alwaysEnabled) b.disabled = busy;
    });
  }

  function showMessage(text, kind = "", details = []) {
    const el = $("message");
    el.textContent = "";
    el.hidden = false;
    el.className = "message" + (kind ? " " + kind : "");
    const p = document.createElement("div");
    p.textContent = text;
    el.appendChild(p);
    for (const d of details) {
      const line = document.createElement("div");
      line.className = "hash-change";
      line.textContent = d;
      el.appendChild(line);
    }
  }

  function clearMessage() {
    const el = $("message");
    el.hidden = true;
    el.textContent = "";
  }

  function showUploadMessage(text, kind = "") {
    const el = $("uploadMessage");
    el.textContent = "";
    el.hidden = false;
    el.className = "message" + (kind ? " " + kind : "");
    const p = document.createElement("div");
    p.textContent = text;
    el.appendChild(p);
  }

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    if (state.token) headers.set("X-Torrent-Web-Token", state.token);
    const resp = await fetch(path, { ...options, headers });
    const text = await resp.text();
    let data = null;
    if (text) {
      try {
        data = JSON.parse(text);
      } catch (err) {
        data = { error: text };
      }
    }
    if (!resp.ok) {
      throw new Error((data && data.error) || `${resp.status} ${resp.statusText}`);
    }
    return data;
  }

  function addCard(label, value, mono = false) {
    const card = document.createElement("div");
    card.className = "card";
    const l = document.createElement("div");
    l.className = "label";
    l.textContent = label;
    const v = document.createElement("div");
    v.className = "value" + (mono ? " mono" : "");
    v.textContent = value == null || value === "" ? "—" : String(value);
    card.append(l, v);
    $("summary").appendChild(card);
  }

  function renderSummary(m) {
    const summary = $("summary");
    summary.textContent = "";
    addCard("Имя", m.name);
    addCard("Версия", (m.version || []).join("+") || "unknown");
    addCard("Info hash v1", m.info_hash_v1, true);
    addCard("Info hash v2", m.info_hash_v2, true);
    addCard("Piece length", m.piece_length ? `${m.piece_length} (${formatSize(m.piece_length)})` : "—");
    addCard("Куски", m.piece_count);
    addCard("Размер", `${formatSize(m.total_size)} (${m.total_size} B)`);
    addCard("Файлов", m.file_count);
    if (m.total_size_with_padding != null) {
      addCard("С padding", formatSize(m.total_size_with_padding));
    }
    if (m.private != null) addCard("Private", m.private ? "да" : "нет");
    if (m.source) addCard("info.source", m.source);
    if (m.created_by) addCard("Created by", m.created_by);
    if (m.creation_date_rfc3339) addCard("Создан", m.creation_date_rfc3339);
    if (m.dirty && (m.original_info_hash_v1 || m.original_info_hash_v2)) {
      addCard(
        "Исходные хеши",
        [m.original_info_hash_v1, m.original_info_hash_v2].filter(Boolean).join(" / "),
        true
      );
    }
  }

  function renderTrackers(m) {
    const box = $("trackers");
    box.textContent = "";
    const tr = m.trackers || {};
    if (tr.announce) {
      const row = document.createElement("div");
      row.className = "row";
      const k = document.createElement("span");
      k.className = "key";
      k.textContent = "announce";
      const v = document.createElement("span");
      v.className = "mono";
      v.textContent = tr.announce;
      row.append(k, v);
      box.appendChild(row);
    }
    const tiers = tr.tiers || [];
    if (tiers.length === 0 && !tr.announce) {
      const p = document.createElement("div");
      p.className = "muted";
      p.textContent = "Трекеров нет (возможен DHT/PEX).";
      box.appendChild(p);
      return;
    }
    tiers.forEach((tier, i) => {
      const row = document.createElement("div");
      row.className = "row";
      const k = document.createElement("span");
      k.className = "key";
      k.textContent = `tier ${i + 1}`;
      const v = document.createElement("span");
      v.className = "mono";
      v.textContent = (tier || []).join(", ");
      row.append(k, v);
      box.appendChild(row);
    });
  }

  function renderWarnings(m) {
    const box = $("warnings");
    box.textContent = "";
    const warnings = m.warnings || [];
    if (warnings.length === 0) {
      const p = document.createElement("div");
      p.className = "muted";
      p.textContent = "Предупреждений нет.";
      box.appendChild(p);
      return;
    }
    for (const warning of warnings) {
      const item = document.createElement("div");
      item.className = "warn-text";
      item.textContent = "• " + warning;
      box.appendChild(item);
    }
  }

  function renderFiles(m) {
    const body = $("filesBody");
    body.textContent = "";
    const files = m.files || [];
    if (files.length === 0) {
      const tr = document.createElement("tr");
      const td = document.createElement("td");
      td.colSpan = 3;
      td.className = "muted";
      td.textContent = "Файлов нет.";
      tr.appendChild(td);
      body.appendChild(tr);
      return;
    }
    const frag = document.createDocumentFragment();
    const limit = Math.min(files.length, 1000);
    for (let fi = 0; fi < limit; fi++) {
      const f = files[fi];
      const tr = document.createElement("tr");
      const path = document.createElement("td");
      path.className = "path";
      path.textContent = f.path || "";
      const size = document.createElement("td");
      size.className = "num";
      size.textContent = formatSize(f.length);
      const note = document.createElement("td");
      const notes = [];
      if (f.padding) notes.push("padding");
      if (f.attr) notes.push(`attr: ${f.attr}`);
      if (f.pieces_root) notes.push("v2");
      note.textContent = notes.join(", ");
      tr.append(path, size, note);
      frag.appendChild(tr);
    }
    if (files.length > limit) {
      const tr = document.createElement("tr");
      const td = document.createElement("td");
      td.colSpan = 3;
      td.className = "muted";
      td.textContent = `… ещё ${files.length - limit} файлов (полный список есть в JSON)`;
      tr.appendChild(td);
      frag.appendChild(tr);
    }
    body.appendChild(frag);
  }

  function renderCommonForm(m) {
    $("announceInput").value = (m.trackers && m.trackers.announce) || "";
    $("commentInput").value = m.comment || "";
    $("sourceInput").value = m.source || "";
    $("privateSelect").value = "keep";
    $("trackersInput").value = ((m.trackers && m.trackers.tiers) || [])
      .map((tier) => (tier || []).join(", "))
      .join("\n");
  }

  function render() {
    const m = state.metadata;
    if (!m) return;
    $("sessionView").hidden = false;
    $("uploadView").hidden = true;
    $("fileName").textContent = state.filename || m.filename || "torrent";
    $("dirtyBadge").hidden = !m.dirty;
    renderSummary(m);
    renderTrackers(m);
    renderWarnings(m);
    renderFiles(m);
    $("extrasView").textContent = JSON.stringify(m.extra || {}, null, 2);
    $("jsonView").textContent = JSON.stringify(m, null, 2);
    renderCommonForm(m);
  }

  function loadSession(resp) {
    state.sessionId = resp.id;
    state.filename = resp.filename || (resp.metadata && resp.metadata.filename) || "torrent";
    state.metadata = resp.metadata;
    clearMessage();
    render();
    selectTab("info");
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

  async function upload(file) {
    if (!file || state.busy) return;
    setBusy(true);
    showUploadMessage("Загрузка и разбор торрента…");
    try {
      const data = await api(`/api/torrents?name=${encodeURIComponent(file.name || "")}`, {
        method: "POST",
        headers: { "Content-Type": "application/x-bittorrent" },
        body: file,
      });
      loadSession(data);
      showUploadMessage("");
      $("uploadMessage").hidden = true;
    } catch (err) {
      showUploadMessage(err.message || String(err), "error");
    } finally {
      setBusy(false);
    }
  }

  function parseTrackers(text) {
    const tiers = [];
    for (const line of text.split(/\r?\n/)) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      const urls = trimmed
        .split(",")
        .map((u) => u.trim())
        .filter(Boolean);
      if (urls.length) tiers.push(urls);
    }
    return tiers;
  }

  function sameTiers(a, b) {
    if (a.length !== b.length) return false;
    for (let i = 0; i < a.length; i++) {
      if (a[i].length !== b[i].length) return false;
      for (let j = 0; j < a[i].length; j++) {
        if (a[i][j] !== b[i][j]) return false;
      }
    }
    return true;
  }

  function opsTouchInfo(ops) {
    return ops.some((op) => {
      if (["set_source", "unset_source", "set_private", "set_public"].includes(op.op)) return true;
      return op.path === "info" || (op.path && op.path.startsWith("info."));
    });
  }

  function confirmInfo(ops) {
    if (!opsTouchInfo(ops)) return true;
    return window.confirm(
      "Изменение секции info изменит info hash торрента.\n\n" +
        "Для v2/hybrid это также может сделать piece layers неконсистентными.\n\n" +
        "Продолжить?"
    );
  }

  async function applyOps(ops, successText = "Изменения применены.") {
    if (state.busy) return;
    if (!ops.length) {
      showMessage("Нет изменений для применения.", "warn");
      return;
    }
    if (!confirmInfo(ops)) return;
    setBusy(true);
    clearMessage();
    try {
      const resp = await api(`/api/torrents/${encodeURIComponent(state.sessionId)}/edit`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ operations: ops }),
      });
      state.metadata = resp.metadata;
      state.filename = resp.filename || state.filename;
      render();
      const details = [];
      if (resp.info_changed) {
        if (resp.info_hash_v1_before || resp.info_hash_v1_after) {
          details.push(`v1: ${resp.info_hash_v1_before || "—"} → ${resp.info_hash_v1_after || "—"}`);
        }
        if (resp.info_hash_v2_before || resp.info_hash_v2_after) {
          details.push(`v2: ${resp.info_hash_v2_before || "—"} → ${resp.info_hash_v2_after || "—"}`);
        }
        showMessage(successText + " info hash изменился.", "warn", details);
      } else {
        showMessage(successText + " info hash не изменился.", "ok");
      }
    } catch (err) {
      showMessage(err.message || String(err), "error");
    } finally {
      setBusy(false);
    }
  }

  function buildCommonOps() {
    const m = state.metadata || {};
    const ops = [];
    const tr = m.trackers || {};

    const announce = $("announceInput").value;
    if (announce !== (tr.announce || "")) {
      ops.push(announce.trim() ? { op: "set_announce", value: announce } : { op: "unset_announce" });
    }

    const comment = $("commentInput").value;
    if (comment !== (m.comment || "")) {
      ops.push(comment.trim() ? { op: "set_comment", value: comment } : { op: "unset_comment" });
    }

    const source = $("sourceInput").value;
    if (source !== (m.source || "")) {
      ops.push(source.trim() ? { op: "set_source", value: source } : { op: "unset_source" });
    }

    const privateMode = $("privateSelect").value;
    if (privateMode === "private") ops.push({ op: "set_private" });
    if (privateMode === "public") ops.push({ op: "set_public" });

    const tiers = parseTrackers($("trackersInput").value);
    const currentTiers = tr.tiers || [];
    if (!sameTiers(tiers, currentTiers)) {
      ops.push({ op: "set_trackers", tiers });
    }
    return ops;
  }

  async function applyCommon(event) {
    event.preventDefault();
    await applyOps(buildCommonOps(), "Основные поля обновлены.");
  }

  function advancedOp(kind) {
    const path = $("pathInput").value.trim();
    if (!path) {
      showMessage("Укажите путь ключа.", "error");
      return null;
    }
    if (kind === "unset") return { op: "unset", path };
    const type = $("typeSelect").value;
    const value = $("valueInput").value;
    if (type === "json") {
      try {
        JSON.parse(value);
      } catch (err) {
        showMessage("Некорректный JSON: " + err.message, "error");
        return null;
      }
    }
    if (type === "int" && !/^-?\d+$/.test(value.trim())) {
      showMessage("Для типа int нужно целое число.", "error");
      return null;
    }
    return { op: kind, path, type, value };
  }

  async function applyAdvanced(kind) {
    const op = advancedOp(kind);
    if (op) await applyOps([op], "Операция применена.");
  }

  async function downloadTorrent() {
    if (!state.sessionId || state.busy) return;
    setBusy(true);
    try {
      const headers = new Headers();
      headers.set("X-Torrent-Web-Token", state.token);
      const resp = await fetch(`/api/torrents/${encodeURIComponent(state.sessionId)}/download`, { headers });
      if (!resp.ok) {
        let message = `${resp.status} ${resp.statusText}`;
        try {
          const data = await resp.json();
          if (data && data.error) message = data.error;
        } catch (err) {}
        throw new Error(message);
      }
      const blob = await resp.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = state.filename || "torrent";
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 2000);
    } catch (err) {
      showMessage(err.message || String(err), "error");
    } finally {
      setBusy(false);
    }
  }

  async function resetTorrent() {
    if (!state.sessionId || state.busy) return;
    if (!window.confirm("Сбросить все изменения и вернуться к исходному файлу?")) return;
    setBusy(true);
    try {
      const resp = await api(`/api/torrents/${encodeURIComponent(state.sessionId)}/reset`, { method: "POST" });
      state.metadata = resp.metadata;
      state.filename = resp.filename || state.filename;
      render();
      showMessage("Изменения сброшены. Восстановлен исходный торрент.", "ok");
    } catch (err) {
      showMessage(err.message || String(err), "error");
    } finally {
      setBusy(false);
    }
  }

  async function newFile() {
    if (state.busy) return;
    if (state.metadata && state.metadata.dirty && !window.confirm("Есть несохранённые изменения. Открыть другой файл?")) {
      return;
    }
    const id = state.sessionId;
    state.sessionId = null;
    state.metadata = null;
    state.filename = "";
    $("sessionView").hidden = true;
    $("uploadView").hidden = false;
    clearMessage();
    $("fileInput").value = "";
    if (id) {
      try {
        await api(`/api/torrents/${encodeURIComponent(id)}`, { method: "DELETE" });
      } catch (err) {}
    }
  }

  function selectTab(name) {
    document.querySelectorAll(".tab").forEach((b) => b.classList.toggle("active", b.dataset.tab === name));
    $("infoTab").hidden = name !== "info";
    $("editTab").hidden = name !== "edit";
  }

  function setupUpload() {
    const zone = $("dropZone");
    const input = $("fileInput");
    zone.addEventListener("click", () => input.click());
    zone.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        input.click();
      }
    });
    input.addEventListener("change", () => {
      if (input.files && input.files[0]) upload(input.files[0]);
    });
    for (const event of ["dragenter", "dragover"]) {
      zone.addEventListener(event, (e) => {
        e.preventDefault();
        zone.classList.add("drag");
      });
    }
    for (const event of ["dragleave", "drop"]) {
      zone.addEventListener(event, (e) => {
        e.preventDefault();
        zone.classList.remove("drag");
      });
    }
    zone.addEventListener("drop", (e) => {
      const file = e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files[0];
      if (file) upload(file);
    });
  }

  async function init() {
    setupUpload();
    document.querySelectorAll(".tab").forEach((b) => b.addEventListener("click", () => selectTab(b.dataset.tab)));
    $("commonForm").addEventListener("submit", applyCommon);
    $("setKeyButton").addEventListener("click", () => applyAdvanced("set"));
    $("appendKeyButton").addEventListener("click", () => applyAdvanced("append"));
    $("unsetKeyButton").addEventListener("click", () => applyAdvanced("unset"));
    $("downloadButton").addEventListener("click", downloadTorrent);
    $("resetButton").addEventListener("click", resetTorrent);
    $("newButton").addEventListener("click", newFile);

    try {
      const info = await api("/api/info");
      state.token = info.token;
      $("versionLine").textContent = `локальный интерфейс · версия ${info.version}`;
      $("connection").textContent = "готово";
      $("connection").className = "badge ok";
    } catch (err) {
      $("connection").textContent = "нет связи";
      $("connection").className = "badge warn";
      showUploadMessage(err.message || String(err), "error");
    }
  }

  init();
})();
