const DB_NAME = "b2-share-pwa";
const DB_VERSION = 1;
const STORE_NAME = "pending";
const PENDING_KEY = "current";
const MEDIA_ANALYSIS_BYTES = 2 * 1024 * 1024;
const VIDEO_ACTIONS = Object.freeze([
  { kind: "remux", status: "available", profile: "mp4-faststart-remux" },
  { kind: "transcode", status: "deferred", requires: "gpu-capacity" }
]);

const state = {
  session: { authenticated: false },
  file: null,
  mediaPlan: null,
  processingJob: null,
  processingPoll: 0,
  publicUrl: "",
  shares: []
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
    "aliasInput",
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
    "shareButton",
    "historyPanel",
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
  els.uploadForm.addEventListener("submit", (event) => {
    event.preventDefault();
    uploadSelectedFile();
  });
  els.clearButton.addEventListener("click", async () => {
    await clearPending();
    setFile(null);
    state.publicUrl = "";
    els.aliasInput.value = "";
    setStatus("Ready");
    render();
  });
  els.copyButton.addEventListener("click", copyResult);
  els.shareButton.addEventListener("click", shareResult);
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
    const response = await apiFetch("/api/shares", { method: "GET" });
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
  state.mediaPlan = null;
  state.processingJob = null;
  state.publicUrl = "";
  render();
  if (!file) {
    return;
  }
  inspectMediaPlan(file)
    .then((plan) => {
      if (state.file !== file) {
        return;
      }
      state.mediaPlan = plan;
      render();
    })
    .catch(() => {
      if (state.file === file) {
        state.mediaPlan = null;
        render();
      }
    });
}

function render() {
  const user = state.session.user || {};
  els.sessionLabel.textContent = user.email || user.preferred_username || "Signed in";
  els.logoutLink.classList.toggle("hidden", !state.session.authenticated);
  els.uploadButton.disabled = !state.file;
  els.clearButton.disabled = !state.file && !state.publicUrl;
  els.resultPanel.classList.toggle("hidden", !state.publicUrl);
  els.historyPanel.classList.toggle("hidden", !state.session.authenticated || state.shares.length === 0);
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
  const warning = state.mediaPlan && state.mediaPlan.warning ? state.mediaPlan.warning : "";
  els.mediaWarning.classList.toggle("hidden", !warning);
  if (!warning) {
    return;
  }

  const text = document.createElement("span");
  text.textContent = warning;
  els.mediaWarning.append(text);

  const remuxAction = (state.mediaPlan.actions || []).find((action) => action.kind === "remux" && action.status === "available");
  if (!remuxAction || !state.publicUrl) {
    return;
  }

  const job = state.processingJob;
  const isBusy = job && (job.status === "queued" || job.status === "running");
  const button = document.createElement("button");
  button.className = "button secondary compact";
  button.type = "button";
  button.textContent = isBusy ? formatProcessingStatus(job.status) : "Remux";
  button.disabled = isBusy;
  button.addEventListener("click", () => startProcessingJob(remuxAction.profile));
  els.mediaWarning.append(button);
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
    setStatus("Hashing");
    els.uploadButton.disabled = true;
    const sha256 = await sha256File(file);
    setStatus("Creating upload");
    const alias = els.aliasInput.value.trim();
    const createResponse = await apiFetch("/api/uploads", {
      method: "POST",
      body: JSON.stringify({
        filename: file.name || "upload",
        contentType: file.type || "application/octet-stream",
        size: file.size,
        sha256,
        ...(alias ? { alias } : {})
      })
    });
    if (createResponse.alreadyUploaded) {
      state.publicUrl = createResponse.publicUrl;
      await clearPending();
      await loadShares();
      setStatus("Uploaded");
      return;
    }
    setStatus("Uploading");
    const uploadHeaders = new Headers(createResponse.requiredHeaders || {});
    if (!uploadHeaders.has("Content-Type")) {
      uploadHeaders.set("Content-Type", file.type || "application/octet-stream");
    }
    const uploadResponse = await fetch(createResponse.uploadUrl, {
      method: "PUT",
      headers: uploadHeaders,
      body: file,
      mode: "cors"
    });
    if (!uploadResponse.ok) {
      throw new Error(`B2 upload failed with ${uploadResponse.status}`);
    }
    setStatus("Verifying");
    const completeResponse = await apiFetch("/api/uploads/complete", {
      method: "POST",
      body: JSON.stringify({ uploadToken: createResponse.uploadToken })
    });
    state.publicUrl = completeResponse.publicUrl;
    await clearPending();
    await loadShares();
    setStatus(completeResponse.verified ? "Uploaded" : "Uploaded, verification pending");
  } catch (error) {
    setStatus(error.message || "Upload failed.", true);
  } finally {
    render();
  }
}

async function apiFetch(url, options = {}) {
  const response = await fetch(url, {
    credentials: "same-origin",
    ...options,
    headers: {
      "Accept": "application/json",
      "Content-Type": "application/json",
      "X-CSRF-Token": state.session.csrfToken || "",
      ...(options.headers || {})
    }
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

async function sha256File(file) {
  if (!crypto.subtle) {
    throw new Error("SHA-256 is not available in this browser.");
  }
  const buffer = await file.arrayBuffer();
  const digest = await crypto.subtle.digest("SHA-256", buffer);
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

function renderShares() {
  if (!els.historyList) {
    return;
  }
  els.historyList.textContent = "";
  for (const share of state.shares) {
    const item = document.createElement("article");
    item.className = "history-item";

    const title = document.createElement("div");
    title.className = "history-title";
    title.textContent = share.displayFilename || share.slug || "share";

    const meta = document.createElement("div");
    meta.className = "history-meta";
    meta.append(
      historySpan(formatBytes(share.size)),
      historySpan(share.contentType || "application/octet-stream"),
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

    item.append(title, meta, actions);
    els.historyList.append(item);
  }
}

function historySpan(value) {
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

async function copyResult() {
  if (!state.publicUrl) {
    return;
  }
  await navigator.clipboard.writeText(state.publicUrl);
  setStatus("Copied");
}

async function shareResult() {
  if (!state.publicUrl || !navigator.share) {
    await copyResult();
    return;
  }
  await navigator.share({ url: state.publicUrl });
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
    if (state.publicUrl === share.publicUrl) {
      state.publicUrl = "";
    }
    setStatus("Deleted");
    render();
  } catch (error) {
    setStatus(error.message || "Delete failed.", true);
  }
}

async function inspectMediaPlan(file) {
  if (!looksLikeMP4(file)) {
    return null;
  }
  const sample = await file.slice(0, Math.min(file.size, MEDIA_ANALYSIS_BYTES)).arrayBuffer();
  const boxes = parseMP4TopLevelBoxes(sample);
  const mdat = boxes.find((box) => box.type === "mdat");
  const moov = boxes.find((box) => box.type === "moov");
  if (mdat && (!moov || mdat.offset < moov.offset)) {
    return {
      kind: "video/mp4",
      status: "warning",
      code: "mp4_moov_after_mdat",
      warning: "MP4 metadata is at the end; inline players may stall until it is remuxed.",
      actions: VIDEO_ACTIONS
    };
  }
  return null;
}

async function startProcessingJob(profile) {
  const slug = slugFromShareURL(state.publicUrl);
  if (!slug) {
    setStatus("Share URL is missing.", true);
    return;
  }
  try {
    setStatus("Queueing remux");
    const job = await apiFetch(`/api/shares/${encodeURIComponent(slug)}/processing-jobs`, {
      method: "POST",
      body: JSON.stringify({ profile })
    });
    state.processingJob = job;
    render();
    pollProcessingJob(job.jobId);
  } catch (error) {
    setStatus(error.message || "Remux failed to start.", true);
  }
}

async function pollProcessingJob(jobId) {
  clearProcessingPoll();
  if (!jobId) {
    return;
  }
  try {
    const job = await apiFetch(`/api/processing-jobs/${encodeURIComponent(jobId)}`, { method: "GET" });
    state.processingJob = job;
    if (job.status === "completed") {
      state.mediaPlan = null;
      state.processingJob = null;
      await loadShares();
      setStatus("Remuxed");
      render();
      return;
    }
    if (job.status === "failed") {
      setStatus(job.error || "Remux failed.", true);
      render();
      return;
    }
    setStatus(formatProcessingStatus(job.status));
    render();
    state.processingPoll = window.setTimeout(() => pollProcessingJob(jobId), 2500);
  } catch (error) {
    setStatus(error.message || "Remux status unavailable.", true);
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
      return "Remux queued";
    case "running":
      return "Remuxing";
    case "completed":
      return "Remuxed";
    case "failed":
      return "Remux failed";
    default:
      return "Remux";
  }
}

function slugFromShareURL(value) {
  if (!value) {
    return "";
  }
  try {
    const parsed = new URL(value, location.origin);
    if (parsed.origin !== location.origin || !parsed.pathname.startsWith("/s/")) {
      return "";
    }
    const slug = decodeURIComponent(parsed.pathname.slice(3));
    return slug && !slug.includes("/") ? slug : "";
  } catch (error) {
    return "";
  }
}

function looksLikeMP4(file) {
  const name = (file.name || "").toLowerCase();
  return file.type === "video/mp4" || name.endsWith(".mp4") || name.endsWith(".m4v");
}

function parseMP4TopLevelBoxes(buffer) {
  const view = new DataView(buffer);
  const boxes = [];
  let offset = 0;
  while (offset + 8 <= view.byteLength) {
    let size = view.getUint32(offset);
    const type = readBoxType(view, offset + 4);
    let headerSize = 8;
    if (size === 1 && offset + 16 <= view.byteLength) {
      const high = view.getUint32(offset + 8);
      const low = view.getUint32(offset + 12);
      size = high * 2 ** 32 + low;
      headerSize = 16;
    } else if (size === 0) {
      size = view.byteLength - offset;
    }
    if (!type || size < headerSize) {
      break;
    }
    boxes.push({ type, offset, size });
    if (offset + size > view.byteLength) {
      break;
    }
    offset += size;
  }
  return boxes;
}

function readBoxType(view, offset) {
  let value = "";
  for (let index = 0; index < 4; index += 1) {
    const code = view.getUint8(offset + index);
    if (code < 32 || code > 126) {
      return "";
    }
    value += String.fromCharCode(code);
  }
  return value;
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
