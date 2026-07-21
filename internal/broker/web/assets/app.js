const DB_NAME = "b2-share-pwa";
const DB_VERSION = 1;
const STORE_NAME = "pending";
const PENDING_KEY = "current";

const state = {
  session: { authenticated: false },
  file: null,
  nameBase: "",
  nameExtension: "",
  processingJob: null,
  processingPoll: 0,
  historySearch: "",
  historySearchPoll: 0,
  publicUrl: "",
  shares: [],
  renamingSlug: "",
  renameBase: "",
  renameExtension: ""
};

const els = {};

window.addEventListener("DOMContentLoaded", init);

async function init() {
  bindElements();
  bindEvents();
  await registerServiceWorker();
  await refreshSession();
  if (!state.session.authenticated) {
    redirectToLogin();
    return;
  }
  document.body.classList.remove("auth-pending");
  await loadPendingShare();
  await loadShares();
  render();
}

function bindElements() {
  for (const id of [
    "sessionLabel",
    "logoutLink",
    "uploadForm",
    "fileInput",
    "nameInput",
    "nameExtension",
    "nameHint",
    "dropzone",
    "fileTitle",
    "fileMeta",
    "mediaWarning",
    "statusText",
    "uploadButton",
    "clearButton",
    "resultPanel",
    "resultUrl",
    "copyButton",
    "historyPanel",
    "historySearch",
    "historyList"
  ]) {
    els[id] = document.getElementById(id);
  }
}

function bindEvents() {
  els.fileInput.addEventListener("change", () => {
    const files = Array.from(els.fileInput.files || []);
    if (files.length === 1) {
      setFile(files[0]);
      setStatus("Ready");
    } else if (files.length > 1) {
      setFile(null);
      setStatus("Share one file at a time.", true);
    }
  });
  els.nameInput.addEventListener("input", () => {
    state.nameBase = els.nameInput.value;
  });
  els.uploadForm.addEventListener("submit", (event) => {
    event.preventDefault();
    uploadSelectedFile();
  });
  els.clearButton.addEventListener("click", async () => {
    await clearPending();
    setFile(null);
    state.publicUrl = "";
    setStatus("Ready");
    render();
  });
  els.copyButton.addEventListener("click", copyResult);
  els.historySearch.addEventListener("input", () => {
    state.historySearch = els.historySearch.value;
    scheduleHistorySearch();
  });
  for (const eventName of ["dragenter", "dragover"]) {
    els.dropzone.addEventListener(eventName, (event) => {
      event.preventDefault();
      els.dropzone.classList.add("dragging");
    });
  }
  for (const eventName of ["dragleave", "drop"]) {
    els.dropzone.addEventListener(eventName, () => {
      els.dropzone.classList.remove("dragging");
    });
  }
  els.dropzone.addEventListener("drop", (event) => {
    event.preventDefault();
    const files = Array.from(event.dataTransfer.files || []);
    if (files.length === 1) {
      setFile(files[0]);
      setStatus("Ready");
    } else {
      setFile(null);
      setStatus("Share one file at a time.", true);
    }
  });
}

async function registerServiceWorker() {
  if (!("serviceWorker" in navigator)) {
    return;
  }
  try {
    await navigator.serviceWorker.register("/sw.js");
  } catch (error) {
    setStatus("Share target unavailable.", true);
  }
}

async function refreshSession() {
  const response = await fetch("/api/session", {
    credentials: "same-origin",
    headers: { "Accept": "application/json" }
  });
  state.session = await response.json();
}

function redirectToLogin() {
  els.sessionLabel.textContent = "Signing in";
  const returnTo = location.pathname + location.search;
  location.replace(`/auth/login?return_to=${encodeURIComponent(returnTo)}`);
}

async function loadShares() {
  if (!state.session.authenticated) {
    state.shares = [];
    return;
  }
  try {
    const params = new URLSearchParams();
    const query = state.historySearch.trim();
    if (query) {
      params.set("q", query);
    }
    const queryString = params.toString();
    const path = queryString ? `/api/shares?${queryString}` : "/api/shares";
    const response = await apiFetch(path, { method: "GET" });
    state.shares = response.shares || [];
  } catch (error) {
    state.shares = [];
  }
}

async function loadPendingShare() {
  const pending = await getPending();
  if (!pending) {
    return;
  }
  if (pending.kind === "error") {
    setStatus(pending.message || "The shared file could not be opened.", true);
    await clearPending();
    return;
  }
  if (pending.kind === "file" && pending.file) {
    setFile(pending.file);
    setStatus("Ready");
  }
}

function setFile(file) {
  clearProcessingPoll();
  state.file = file;
  if (file) {
    const suggested = suggestedShareName(file);
    state.nameBase = suggested.base;
    state.nameExtension = suggested.extension;
  } else {
    state.nameBase = "";
    state.nameExtension = "";
  }
  state.processingJob = null;
  state.publicUrl = "";
  render();
}

function render() {
  const user = state.session.user || {};
  els.sessionLabel.textContent = user.email || user.preferred_username || "Signed in";
  els.logoutLink.classList.toggle("hidden", !state.session.authenticated);
  els.uploadButton.disabled = !state.file;
  els.clearButton.disabled = !state.file && !state.publicUrl;
  els.nameInput.disabled = !state.file;
  els.nameInput.value = state.nameBase;
  els.nameExtension.textContent = state.nameExtension;
  els.nameHint.textContent = state.file
    ? `The ${state.nameExtension} extension is fixed.`
    : "Choose a file to generate a public name.";
  els.resultPanel.classList.toggle("hidden", !state.publicUrl);
  els.historyPanel.classList.toggle("hidden", !state.session.authenticated);
  els.resultUrl.textContent = state.publicUrl;
  renderMediaWarning();
  if (state.file) {
    els.fileTitle.textContent = state.file.name || "upload";
    els.fileMeta.textContent = `${formatBytes(state.file.size)} - ${state.file.type || "application/octet-stream"}`;
  } else {
    els.fileTitle.textContent = "Choose one file";
    els.fileMeta.textContent = "No file selected";
  }
  renderShares();
}

function setStatus(message, isError = false) {
  els.statusText.textContent = message;
  els.statusText.classList.toggle("error", isError);
}

function renderMediaWarning() {
  els.mediaWarning.textContent = "";
  els.mediaWarning.classList.add("hidden");
}

async function uploadSelectedFile() {
  const file = state.file;
  if (!file) {
    return;
  }
  if (!state.session.authenticated) {
    await putPending({
      kind: "file",
      file,
      name: file.name || "upload",
      type: file.type || "application/octet-stream",
      size: file.size,
      receivedAt: Date.now()
    });
    location.assign(`/auth/login?return_to=${encodeURIComponent("/share")}`);
    return;
  }
  try {
    setStatus("Uploading");
    els.uploadButton.disabled = true;
    const form = new FormData();
    form.append("file", file, file.name || "upload");
    form.append("name", shareName(state.nameBase, state.nameExtension));
    const createResponse = await apiFetch("/api/uploads", {
      method: "POST",
      body: form
    });
    state.publicUrl = createResponse.shareUrl;
    state.processingJob = createResponse;
    await clearPending();
    await loadShares();
    setStatus(formatProcessingStatus(createResponse.status));
    pollUploadJob(createResponse.jobId);
  } catch (error) {
    setStatus(error.message || "Upload failed.", true);
  } finally {
    render();
  }
}

async function apiFetch(url, options = {}) {
  const headers = new Headers(options.headers || {});
  headers.set("Accept", "application/json");
  headers.set("X-CSRF-Token", state.session.csrfToken || "");
  if (!(options.body instanceof FormData) && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  const response = await fetch(url, {
    credentials: "same-origin",
    ...options,
    headers
  });
  if (response.status === 401) {
    location.assign(`/auth/login?return_to=${encodeURIComponent("/share")}`);
    throw new Error("Sign in required.");
  }
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(body.error || `Request failed with ${response.status}`);
  }
  return body;
}

function renderShares() {
  if (!els.historyList) {
    return;
  }
  els.historyList.textContent = "";
  if (state.shares.length === 0) {
    const empty = document.createElement("p");
    empty.className = "history-empty";
    empty.textContent = state.historySearch.trim() ? "No matching shares" : "No shares yet";
    els.historyList.append(empty);
    return;
  }
  for (const share of state.shares) {
    const item = document.createElement("article");
    item.className = "history-item";

    if (state.renamingSlug === share.slug) {
      item.append(renderRenameForm(share));
    } else {
      const title = document.createElement("button");
      title.className = "history-title history-name-button";
      title.type = "button";
      title.textContent = share.slug || share.displayFilename || "share";
      title.title = "Rename share";
      title.addEventListener("click", () => beginRename(share));
      item.append(title);
    }

    const meta = document.createElement("div");
    meta.className = "history-meta";
    meta.append(
      historySpan(formatShareStatus(share.status)),
      historySpan(formatShareSize(share)),
      historySpan(share.contentType || ""),
      historySpan(`${share.redirectCount || 0} opens`),
      historySpan(formatDate(share.updatedAt))
    );

    const links = document.createElement("div");
    links.className = "history-links";
    links.append(historyLink("Share", share.publicUrl), historyLink("B2", share.b2Url));

    const deleteButton = document.createElement("button");
    deleteButton.className = "button danger compact";
    deleteButton.type = "button";
    deleteButton.textContent = "Delete";
    deleteButton.addEventListener("click", () => deleteShare(share));

    const actions = document.createElement("div");
    actions.className = "history-actions";
    actions.append(links, deleteButton);

    item.append(meta);
    if (share.error) {
      const error = document.createElement("div");
      error.className = "history-error";
      error.textContent = share.error;
      item.append(error);
    }
    item.append(actions);
    els.historyList.append(item);
  }
}

function beginRename(share) {
  state.renamingSlug = share.slug;
  state.renameExtension = shareNameExtension(share.slug);
  state.renameBase = shareNameBase(share.slug || share.displayFilename, state.renameExtension);
  render();
  window.requestAnimationFrame(() => {
    const input = document.getElementById("renameInput");
    if (input) {
      input.focus();
      input.select();
    }
  });
}

function renderRenameForm(share) {
  const form = document.createElement("form");
  form.className = "history-rename";
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    await renameShare(share);
  });

  const control = document.createElement("span");
  control.className = "name-control";
  const input = document.createElement("input");
  input.id = "renameInput";
  input.type = "text";
  input.autocomplete = "off";
  input.spellcheck = false;
  input.value = state.renameBase;
  input.setAttribute("aria-label", "Share name");
  input.addEventListener("input", () => {
    state.renameBase = input.value;
  });
  const extension = document.createElement("output");
  extension.className = "name-extension";
  extension.textContent = state.renameExtension;
  control.append(input, extension);

  const saveButton = document.createElement("button");
  saveButton.className = "button primary compact";
  saveButton.type = "submit";
  saveButton.textContent = "Save";

  const cancelButton = document.createElement("button");
  cancelButton.className = "button secondary compact";
  cancelButton.type = "button";
  cancelButton.textContent = "Cancel";
  cancelButton.addEventListener("click", () => {
    state.renamingSlug = "";
    state.renameBase = "";
    state.renameExtension = "";
    render();
  });

  form.append(control, saveButton, cancelButton);
  return form;
}

async function renameShare(share) {
  if (!share || !share.slug) {
    return;
  }
  try {
    const renamed = await apiFetch(`/api/shares/${encodeURIComponent(share.slug)}`, {
      method: "PATCH",
      body: JSON.stringify({ name: shareName(state.renameBase, state.renameExtension) })
    });
    if (state.publicUrl === share.publicUrl) {
      state.publicUrl = renamed.publicUrl;
    }
    if (state.processingJob && state.processingJob.slug === share.slug) {
      state.processingJob.slug = renamed.slug;
      state.processingJob.shareUrl = renamed.publicUrl;
    }
    state.renamingSlug = "";
    state.renameBase = "";
    state.renameExtension = "";
    await loadShares();
    setStatus("Renamed");
    render();
  } catch (error) {
    setStatus(error.message || "Rename failed.", true);
  }
}

function scheduleHistorySearch() {
  if (state.historySearchPoll) {
    window.clearTimeout(state.historySearchPoll);
  }
  state.historySearchPoll = window.setTimeout(async () => {
    state.historySearchPoll = 0;
    await loadShares();
    render();
  }, 250);
}

function historySpan(value) {
  if (!value) {
    return document.createDocumentFragment();
  }
  const span = document.createElement("span");
  span.textContent = value;
  return span;
}

function historyLink(label, href) {
  const link = document.createElement("a");
  link.textContent = label;
  link.href = href || "#";
  if (!href) {
    link.hidden = true;
  }
  return link;
}

function suggestedShareName(file) {
  const extension = finalExtensionForFile(file);
  const filename = String(file.name || "upload").replaceAll("\\", "/").split("/").pop();
  const base = filename.replace(/\.[^.]*$/, "");
  let normalized = "";
  let lastWasSeparator = false;
  for (const character of base) {
    if (character === "." || character === "-") {
      normalized += character;
      lastWasSeparator = false;
    } else if (character === "_" || /\s/.test(character)) {
      if (!lastWasSeparator) {
        normalized += "_";
        lastWasSeparator = true;
      }
    } else if (/^[A-Za-z0-9]$/.test(character)) {
      normalized += character;
      lastWasSeparator = false;
    }
  }
  normalized = normalized
    .replace(/^[._-]+|[._-]+$/g, "")
    .replace(/[-_]{2,}/g, "_")
    .toLowerCase();
  if (!normalized) {
    normalized = "share";
  }
  normalized = normalized.slice(0, 120).replace(/[._-]+$/g, "") || "share";
  return { base: `${normalized}-${randomHex(8)}`, extension };
}

function finalExtensionForFile(file) {
  const filename = String(file.name || "").toLowerCase();
  const contentType = String(file.type || "").toLowerCase().split(";", 1)[0];
  const videoExtensions = [".mp4", ".m4v", ".mov", ".mkv", ".webm", ".avi"];
  if (contentType.startsWith("video/") || videoExtensions.some((extension) => filename.endsWith(extension))) {
    return ".mp4";
  }
  const match = filename.match(/\.([a-z0-9]{1,16})$/);
  if (match) {
    return `.${match[1]}`;
  }
  const extensions = {
    "image/jpeg": ".jpg",
    "image/png": ".png",
    "image/gif": ".gif",
    "image/webp": ".webp",
    "image/heic": ".heic",
    "image/heif": ".heif",
    "application/pdf": ".pdf",
    "text/plain": ".txt"
  };
  return extensions[contentType] || ".bin";
}

function randomHex(byteLength) {
  const bytes = new Uint8Array(byteLength);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (value) => value.toString(16).padStart(2, "0")).join("");
}

function shareName(base, extension) {
  return `${String(base || "").trim() || "share"}${extension || ""}`;
}

function shareNameExtension(slug) {
  const match = String(slug || "").match(/\.[A-Za-z0-9]{1,16}$/);
  return match ? match[0].toLowerCase() : "";
}

function shareNameBase(name, extension) {
  const value = String(name || "").trim();
  if (extension && value.toLowerCase().endsWith(extension)) {
    return value.slice(0, -extension.length) || "share";
  }
  return value.replace(/\.[^.]*$/, "") || "share";
}

async function copyResult() {
  if (!state.publicUrl) {
    return;
  }
  await navigator.clipboard.writeText(state.publicUrl);
  setStatus("Copied");
}

async function deleteShare(share) {
  if (!share || !share.slug) {
    return;
  }
  if (!window.confirm("Delete this share URL?")) {
    return;
  }
  try {
    await apiFetch(`/api/shares/${encodeURIComponent(share.slug)}`, { method: "DELETE" });
    state.shares = state.shares.filter((item) => item.slug !== share.slug);
    if (state.renamingSlug === share.slug) {
      state.renamingSlug = "";
      state.renameBase = "";
      state.renameExtension = "";
    }
    if (state.publicUrl === share.publicUrl) {
      state.publicUrl = "";
    }
    setStatus("Deleted");
    render();
  } catch (error) {
    setStatus(error.message || "Delete failed.", true);
  }
}

async function pollUploadJob(jobId) {
  clearProcessingPoll();
  if (!jobId) {
    return;
  }
  try {
    const job = await apiFetch(`/api/uploads/${encodeURIComponent(jobId)}`, { method: "GET" });
    state.processingJob = job;
    state.publicUrl = job.shareUrl || state.publicUrl;
    if (job.status === "completed") {
      state.processingJob = null;
      await loadShares();
      setStatus("Uploaded");
      render();
      return;
    }
    if (job.status === "failed") {
      await loadShares();
      setStatus(job.error || "Processing failed.", true);
      render();
      return;
    }
    if (job.status === "canceled") {
      await loadShares();
      setStatus("Canceled", true);
      render();
      return;
    }
    setStatus(formatProcessingStatus(job.status));
    render();
    state.processingPoll = window.setTimeout(() => pollUploadJob(jobId), 2500);
  } catch (error) {
    setStatus(error.message || "Upload status unavailable.", true);
    render();
  }
}

function clearProcessingPoll() {
  if (state.processingPoll) {
    window.clearTimeout(state.processingPoll);
    state.processingPoll = 0;
  }
}

function formatProcessingStatus(status) {
  switch (status) {
    case "queued":
      return "Queued";
    case "running":
      return "Processing";
    case "completed":
      return "Uploaded";
    case "failed":
      return "Failed";
    case "canceled":
      return "Canceled";
    default:
      return "Processing";
  }
}

function formatShareStatus(status) {
  switch (status) {
    case "ready":
      return "Ready";
    case "failed":
      return "Failed";
    case "pending":
    case "":
    default:
      return "Processing";
  }
}

function formatShareSize(share) {
  if (share.size) {
    return formatBytes(share.size);
  }
  if (share.status === "pending") {
    return "Waiting";
  }
  return "";
}

function formatBytes(bytes) {
  if (!Number.isFinite(bytes) || bytes <= 0) {
    return "0 B";
  }
  const units = ["B", "KB", "MB", "GB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(value >= 10 || unit === 0 ? 0 : 1)} ${units[unit]}`;
}

function formatDate(value) {
  if (!value) {
    return "";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "";
  }
  return date.toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
}

function openDB() {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(DB_NAME, DB_VERSION);
    request.onupgradeneeded = () => {
      request.result.createObjectStore(STORE_NAME);
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

async function getPending() {
  if (!("indexedDB" in window)) {
    return null;
  }
  const db = await openDB();
  try {
    return await new Promise((resolve, reject) => {
      const tx = db.transaction(STORE_NAME, "readonly");
      const request = tx.objectStore(STORE_NAME).get(PENDING_KEY);
      request.onsuccess = () => resolve(request.result || null);
      request.onerror = () => reject(request.error);
    });
  } finally {
    db.close();
  }
}

async function putPending(value) {
  if (!("indexedDB" in window)) {
    return;
  }
  const db = await openDB();
  try {
    await new Promise((resolve, reject) => {
      const tx = db.transaction(STORE_NAME, "readwrite");
      tx.objectStore(STORE_NAME).put(value, PENDING_KEY);
      tx.oncomplete = resolve;
      tx.onerror = () => reject(tx.error);
    });
  } finally {
    db.close();
  }
}

async function clearPending() {
  if (!("indexedDB" in window)) {
    return;
  }
  const db = await openDB();
  try {
    await new Promise((resolve, reject) => {
      const tx = db.transaction(STORE_NAME, "readwrite");
      tx.objectStore(STORE_NAME).delete(PENDING_KEY);
      tx.oncomplete = resolve;
      tx.onerror = () => reject(tx.error);
    });
  } finally {
    db.close();
  }
}
