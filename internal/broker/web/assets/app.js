const DB_NAME = "b2-share-pwa";
const DB_VERSION = 1;
const STORE_NAME = "pending";
const PENDING_KEY = "current";

const state = {
  session: { authenticated: false },
  file: null,
  publicUrl: ""
};

const els = {};

window.addEventListener("DOMContentLoaded", init);

async function init() {
  bindElements();
  bindEvents();
  await registerServiceWorker();
  await loadPendingShare();
  await refreshSession();
  render();
}

function bindElements() {
  for (const id of [
    "sessionLabel",
    "loginLink",
    "logoutLink",
    "uploadForm",
    "fileInput",
    "dropzone",
    "fileTitle",
    "fileMeta",
    "statusText",
    "uploadButton",
    "clearButton",
    "resultPanel",
    "resultUrl",
    "copyButton",
    "shareButton"
  ]) {
    els[id] = document.getElementById(id);
  }
}

function bindEvents() {
  els.loginLink.href = `/auth/login?return_to=${encodeURIComponent(location.pathname + location.search)}`;
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
  state.file = file;
  state.publicUrl = "";
  render();
}

function render() {
  const user = state.session.user || {};
  els.sessionLabel.textContent = state.session.authenticated ? (user.email || user.preferred_username || "Signed in") : "Signed out";
  els.loginLink.classList.toggle("hidden", state.session.authenticated);
  els.logoutLink.classList.toggle("hidden", !state.session.authenticated);
  els.uploadButton.disabled = !state.file;
  els.clearButton.disabled = !state.file && !state.publicUrl;
  els.resultPanel.classList.toggle("hidden", !state.publicUrl);
  els.resultUrl.textContent = state.publicUrl;
  if (state.file) {
    els.fileTitle.textContent = state.file.name || "upload";
    els.fileMeta.textContent = `${formatBytes(state.file.size)} - ${state.file.type || "application/octet-stream"}`;
  } else {
    els.fileTitle.textContent = "Choose one file";
    els.fileMeta.textContent = "No file selected";
  }
}

function setStatus(message, isError = false) {
  els.statusText.textContent = message;
  els.statusText.classList.toggle("error", isError);
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
    setStatus("Creating upload");
    els.uploadButton.disabled = true;
    const createResponse = await apiFetch("/api/uploads", {
      method: "POST",
      body: JSON.stringify({
        filename: file.name || "upload",
        contentType: file.type || "application/octet-stream",
        size: file.size
      })
    });
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
    setStatus(completeResponse.verified ? "Uploaded" : "Uploaded, verification pending");
  } catch (error) {
    setStatus(error.message || "Upload failed.", true);
  } finally {
    render();
  }
}

async function apiFetch(url, options) {
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
