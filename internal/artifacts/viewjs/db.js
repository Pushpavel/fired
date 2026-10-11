// The artifact's db, for its page:
//
//   import { db } from "/.fired/db.js";
//   const today = await db.doc("stats/today").get();      // {id, path, data, rev, …} or null
//   const open = await db.collection("tasks").where("done", "==", false).orderBy("due").get();
//   const stop = db.collection("tasks").onSnapshot(docs => render(docs));
//   if (db.canWrite) await db.doc("tasks/t1").update({ done: true });
//
// The artifact's owner writes; everyone else reads (not _private/).
// `job` (below) runs the artifact's job from its owner's page.
// Documents are JSON objects at paths with an even number of segments
// ("collection/doc", "a/b/c/d"); a collection is the documents directly
// under it.

const root = "/.fired/db";
const canWrite = __CAN_WRITE__;

const enc = (p) => p.split("/").filter(Boolean).map(encodeURIComponent).join("/");

class DBError extends Error {
  constructor(status, body) {
    super(body?.error?.message || `db request failed (${status})`);
    this.status = status;
    this.code = body?.error?.code || "http_error";
  }
}

async function call(method, path, body, headers = {}, query = "") {
  const res = await fetch(`${root}/${enc(path)}${query ? `?${query}` : ""}`, {
    method,
    headers: body === undefined ? headers : { "Content-Type": "application/json", ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin",
  });
  if (res.status === 204) return null;
  const json = await res.json().catch(() => null);
  if (!res.ok) throw new DBError(res.status, json);
  return json;
}

const withId = (d) => (d ? { id: d.path.slice(d.path.lastIndexOf("/") + 1), ...d } : d);

function watch(url, onData, onError) {
  const es = new EventSource(url);
  es.addEventListener("snapshot", (e) => onData(JSON.parse(e.data)));
  es.addEventListener("error", (e) => {
    if (e.data) {
      onError?.(new Error(JSON.parse(e.data)));
      es.close();
    }
    // Otherwise the connection dropped: EventSource reconnects, and the
    // first event after it is a fresh snapshot.
  });
  return () => es.close();
}

class DocRef {
  constructor(path) {
    this.path = path.replace(/^\/+|\/+$/g, "");
    this.id = this.path.slice(this.path.lastIndexOf("/") + 1);
  }
  collection(name) { return new CollectionRef(`${this.path}/${name}`); }
  async get() {
    try {
      return withId(await call("GET", this.path));
    } catch (e) {
      if (e.code === "no_document") return null;
      throw e;
    }
  }
  // set replaces the document; update merges into it (null removes a key)
  // and creates it if it's missing. {rev} writes only if the document is
  // still at that rev (-1: only if it doesn't exist).
  async set(data, { rev } = {}) { return withId(await call("PUT", this.path, data, rev ? { "If-Match": String(rev) } : {})); }
  async update(patch, { rev } = {}) { return withId(await call("PATCH", this.path, patch, rev ? { "If-Match": String(rev) } : {})); }
  async delete({ rev } = {}) { await call("DELETE", this.path, undefined, rev ? { "If-Match": String(rev) } : {}); }
  // onSnapshot calls fn with the document now and after every change
  // (null while it doesn't exist). It returns a function that stops it.
  onSnapshot(fn, onError) {
    return watch(`${root}/${enc(this.path)}?watch`, (d) => fn(withId(d)), onError);
  }
}

class Query {
  constructor(path, params = new URLSearchParams()) {
    this.path = path.replace(/^\/+|\/+$/g, "");
    this.params = params;
  }
  #with(k, v, append) {
    const p = new URLSearchParams(this.params);
    append ? p.append(k, v) : p.set(k, v);
    return new Query(this.path, p);
  }
  // where("done", "==", false); ops: == != < <= > >=; field may be dotted (a.b).
  where(field, op, value) { return this.#with("where", `${field}${op}${JSON.stringify(value)}`, true); }
  orderBy(field, dir = "asc") { return this.#with("order", dir === "desc" ? `-${field}` : field); }
  limit(n) { return this.#with("limit", String(n)); }
  async get() {
    const out = [];
    let after = "";
    for (;;) {
      const p = new URLSearchParams(this.params);
      if (after) p.set("after", after);
      const page = await call("GET", this.path, undefined, {}, p.toString());
      out.push(...page.docs.map(withId));
      if (!page.next || this.params.has("limit")) return out;
      after = page.next;
    }
  }
  onSnapshot(fn, onError) {
    const p = new URLSearchParams(this.params);
    p.set("watch", "");
    return watch(`${root}/${enc(this.path)}?${p}`, (list) => fn(list.docs.map(withId)), onError);
  }
}

// newId is 20 random letters and digits. (crypto.randomUUID needs a secure
// context; artifacts may be served over plain HTTP.)
function newId() {
  const abc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
  return Array.from(crypto.getRandomValues(new Uint8Array(20)), b => abc[b % abc.length]).join("");
}

class CollectionRef extends Query {
  doc(id) { return new DocRef(`${this.path}/${id ?? newId()}`); }
  // add creates a document with a new id.
  async add(data) { return this.doc().set(data, { rev: -1 }); }
}

// The artifact's job, if it has one: anyone can see its status (when it
// runs next); its owner can run it now, with a payload the job reads from
// $TRIGGER_FILE (trigger type "page"). What it produces arrives in the db.
export const job = {
  canRun: canWrite,
  async status() {
    const res = await fetch("/.fired/job", { credentials: "same-origin" });
    const json = await res.json().catch(() => null);
    if (res.status === 404) return null;
    if (!res.ok) throw new DBError(res.status, json);
    return json;
  },
  async run(payload) {
    const res = await fetch("/.fired/job/runs", { method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload === undefined ? {} : { payload }) });
    const json = await res.json().catch(() => null);
    if (!res.ok) throw new DBError(res.status, json);
    return json;
  },
};

export const db = {
  canWrite,
  doc: (path) => new DocRef(path),
  collection: (path) => new CollectionRef(path),
  // batch applies writes ({op: "set"|"update"|"delete", path, data, rev}) in
  // order, all or none.
  async batch(writes) { return (await call("POST", "", { writes })).docs.map(withId); },
};
export default db;
