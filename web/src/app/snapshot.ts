// A tiny IndexedDB key-value helper for the node UI's last known network
// view. Values are plain JSON (protojson of the list rows) plus the time they
// were taken. Nothing secret goes here: no tokens, no CSRF values.

const DB = 'mycel-ui';
const STORE = 'snapshot';

export interface Snapshot<T = unknown> {
  at: string; // RFC 3339
  value: T;
}

function open(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    if (typeof indexedDB === 'undefined') {
      reject(new Error('IndexedDB unavailable'));
      return;
    }
    const req = indexedDB.open(DB, 1);
    req.onupgradeneeded = () => {
      if (!req.result.objectStoreNames.contains(STORE)) req.result.createObjectStore(STORE);
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error ?? new Error('IndexedDB open failed'));
  });
}

async function tx<R>(mode: IDBTransactionMode, fn: (s: IDBObjectStore) => IDBRequest<R>): Promise<R> {
  const db = await open();
  try {
    return await new Promise<R>((resolve, reject) => {
      const t = db.transaction(STORE, mode);
      const req = fn(t.objectStore(STORE));
      t.oncomplete = () => resolve(req.result);
      t.onerror = () => reject(t.error ?? new Error('IndexedDB transaction failed'));
      t.onabort = () => reject(t.error ?? new Error('IndexedDB transaction aborted'));
    });
  } finally {
    db.close();
  }
}

export async function saveSnapshot<T>(key: string, value: T, at = new Date()): Promise<void> {
  const snap: Snapshot<T> = { at: at.toISOString(), value };
  await tx('readwrite', (s) => s.put(snap, key));
}

export async function loadSnapshot<T>(key: string): Promise<Snapshot<T> | null> {
  try {
    const v = await tx<Snapshot<T> | undefined>('readonly', (s) => s.get(key) as IDBRequest<Snapshot<T> | undefined>);
    return v ?? null;
  } catch {
    return null;
  }
}

export async function clearSnapshots(): Promise<void> {
  try {
    await tx('readwrite', (s) => s.clear());
  } catch {
    /* nothing stored */
  }
}
