interface SiteInput {
  site: {
    origin: string;
    clear_local_storage?: boolean;
    local_storage_remove?: string[];
    local_storage?: Record<string, string>;
    indexed_db?: DatabaseSettings[];
  };
  files: Record<string, unknown>;
}

interface DatabaseSettings {
  name: string;
  version?: number;
  delete?: boolean;
  stores?: StoreSettings[];
}

interface StoreSettings {
  name: string;
  key_path?: string | string[];
  auto_increment?: boolean;
  delete?: boolean;
  clear?: boolean;
  records?: RecordSettings[];
  indexes?: IndexSettings[];
}

interface RecordSettings {
  key?: IDBValidKey;
  value: unknown;
  delete?: boolean;
}

interface IndexSettings {
  name: string;
  key_path: string | string[];
  unique?: boolean;
  multi_entry?: boolean;
}

// Keep request handlers installed before the browser dispatches IDB events
function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  const { promise, resolve, reject } = Promise.withResolvers<T>();
  request.onsuccess = () => resolve(request.result);
  request.onerror = () => reject(request.error);
  request.addEventListener(
    "blocked",
    () => reject(new Error("IndexedDB operation blocked")),
    { once: true },
  );
  return promise;
}

function applyStoreSchema(
  db: IDBDatabase,
  upgrade: IDBTransaction,
  settings: StoreSettings,
): void {
  if (settings.delete) {
    if (db.objectStoreNames.contains(settings.name))
      db.deleteObjectStore(settings.name);
    return;
  }
  const store = db.objectStoreNames.contains(settings.name)
    ? upgrade.objectStore(settings.name)
    : db.createObjectStore(settings.name, {
        keyPath: settings.key_path ?? null,
        autoIncrement: settings.auto_increment ?? false,
      });
  for (const index of settings.indexes ?? []) {
    if (!store.indexNames.contains(index.name)) {
      store.createIndex(index.name, index.key_path, {
        unique: index.unique ?? false,
        multiEntry: index.multi_entry ?? false,
      });
    }
  }
}

async function openDatabase(settings: DatabaseSettings): Promise<IDBDatabase> {
  const request = settings.version
    ? indexedDB.open(settings.name, settings.version)
    : indexedDB.open(settings.name);
  let upgradeError: unknown;
  request.onupgradeneeded = () => {
    const upgrade = request.transaction;
    if (!upgrade) throw new Error("Missing IndexedDB upgrade transaction");
    try {
      for (const store of settings.stores ?? [])
        applyStoreSchema(request.result, upgrade, store);
    } catch (error) {
      upgradeError = error;
      upgrade.abort();
    }
  };
  return requestResult(request).catch((error: unknown) => {
    throw upgradeError ?? error;
  });
}

function validateStoreSchema(
  store: IDBObjectStore,
  settings: StoreSettings,
): void {
  if (
    settings.key_path !== undefined &&
    JSON.stringify(store.keyPath) !== JSON.stringify(settings.key_path)
  ) {
    throw new Error(`Existing store ${settings.name} has a different key path`);
  }
  if (settings.auto_increment && !store.autoIncrement) {
    throw new Error(`Existing store ${settings.name} does not auto-increment`);
  }
  for (const index of settings.indexes ?? []) {
    if (!store.indexNames.contains(index.name)) {
      throw new Error(
        `Creating index ${index.name} requires a higher database version`,
      );
    }
    const existing = store.index(index.name);
    if (
      JSON.stringify(existing.keyPath) !== JSON.stringify(index.key_path) ||
      existing.unique !== (index.unique ?? false) ||
      existing.multiEntry !== (index.multi_entry ?? false)
    ) {
      throw new Error(`Existing index ${index.name} has a different schema`);
    }
  }
}

async function applyStore(
  db: IDBDatabase,
  settings: StoreSettings,
): Promise<void> {
  if (settings.delete) {
    if (db.objectStoreNames.contains(settings.name)) {
      throw new Error(
        `Deleting store ${settings.name} requires a higher database version`,
      );
    }
    return;
  }
  if (!db.objectStoreNames.contains(settings.name)) {
    throw new Error(
      `Creating store ${settings.name} requires a higher database version`,
    );
  }
  const tx = db.transaction(settings.name, "readwrite");
  const { promise: complete, resolve, reject } = Promise.withResolvers<void>();
  tx.oncomplete = () => resolve();
  tx.onabort = () =>
    reject(tx.error ?? new Error("IndexedDB transaction aborted"));
  try {
    const store = tx.objectStore(settings.name);
    validateStoreSchema(store, settings);
    if (settings.clear) store.clear();
    for (const record of settings.records ?? []) {
      if (record.delete) {
        if (record.key === undefined)
          throw new Error("Deleting a record requires a key");
        store.delete(record.key);
      } else if (record.key === undefined) store.put(record.value);
      else store.put(record.value, record.key);
    }
  } catch (error) {
    tx.abort();
    await complete.catch(() => {});
    throw error;
  }
  await complete;
}

// Native transactions preserve key encodings, indexes, and structured clone
// values without depending on either engine's private on-disk representation
// https://github.com/mozilla-firefox/firefox/tree/main/dom/indexedDB
// https://github.com/chromium/chromium/tree/main/content/browser/indexed_db
async function applySite(input: SiteInput): Promise<void> {
  const site = input.site;
  if (site.clear_local_storage) localStorage.clear();
  for (const key of site.local_storage_remove ?? [])
    localStorage.removeItem(key);
  for (const [key, value] of Object.entries(site.local_storage ?? {}))
    localStorage.setItem(key, value);
  for (const database of site.indexed_db ?? []) {
    if (database.delete) {
      await requestResult(indexedDB.deleteDatabase(database.name));
      continue;
    }
    const db = await openDatabase(database);
    try {
      for (const store of database.stores ?? []) await applyStore(db, store);
    } finally {
      db.close();
    }
  }
}
