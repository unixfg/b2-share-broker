const DB_NAME = "b2-share-pwa";
const DB_VERSION = 1;
const STORE_NAME = "pending";
const PENDING_KEY = "current";

self.addEventListener("install", (event) => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);
  if (url.origin === self.location.origin && url.pathname === "/share-target" && event.request.method === "POST") {
    event.respondWith(handleShareTarget(event.request));
  }
});

async function handleShareTarget(request) {
  try {
    const form = await request.formData();
    const files = form.getAll("file").filter(isFileLike);
    if (files.length !== 1) {
      await putPending({
        kind: "error",
        message: "Share one file at a time."
      });
      return Response.redirect("/share?shared=1", 303);
    }
    const file = files[0];
    await putPending({
      kind: "file",
      file,
      name: file.name || "upload",
      type: file.type || "application/octet-stream",
      size: file.size,
      receivedAt: Date.now()
    });
    return Response.redirect("/share?shared=1", 303);
  } catch (error) {
    await putPending({
      kind: "error",
      message: "The shared file could not be opened."
    });
    return Response.redirect("/share?shared=1", 303);
  }
}

function isFileLike(value) {
  return value && typeof value === "object" && typeof value.arrayBuffer === "function" && typeof value.name === "string";
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

async function putPending(value) {
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
