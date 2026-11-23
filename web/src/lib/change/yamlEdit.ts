// Edit YAML text by field path and keep everything else as it is: comments,
// key order, quoting and flow style. Scalars are spliced into the source
// text; structural edits re-stringify the document with the style of the
// file (flow padding, final newline).
import { isMap, isScalar, isSeq, parseDocument, stringify, type Document } from 'yaml';

export type Path = (string | number)[];

/** "spec.ports[1].peer.port" → ["spec", "ports", 1, "peer", "port"]. */
export function parsePath(dotted: string): Path {
  const out: Path = [];
  for (const part of dotted.split('.')) {
    if (part === '') continue;
    const re = /([^[\]]+)|\[(\d+)\]/g;
    let m: RegExpExecArray | null;
    while ((m = re.exec(part))) {
      if (m[1] !== undefined) out.push(m[1]);
      else out.push(Number(m[2]));
    }
  }
  return out;
}

/** ["spec", "ports", 1, "peer"] → "spec.ports[1].peer". */
export function formatPath(path: Path): string {
  let s = '';
  for (const p of path) {
    if (typeof p === 'number') s += `[${p}]`;
    else s += s ? `.${p}` : p;
  }
  return s;
}

export interface Parsed {
  data: unknown;
  error?: { message: string; line?: number };
}

/** Parse the first document to plain data. */
export function readData(content: string): Parsed {
  const doc = parseDocument(content, { prettyErrors: false });
  if (doc.errors.length) {
    const e = doc.errors[0];
    const line = e.linePos?.[0]?.line ?? (e.pos ? content.slice(0, e.pos[0]).split('\n').length : undefined);
    return { data: safeJS(doc), error: { message: e.message.split('\n')[0].replace(/ at line \d+, column \d+:?$/, ''), line } };
  }
  return { data: safeJS(doc) };
}

function safeJS(doc: Document): unknown {
  try {
    return doc.toJS({ maxAliasCount: 50 });
  } catch {
    return undefined;
  }
}

type Primitive = string | number | boolean | null;

function isPrimitive(v: unknown): v is Primitive {
  return v === null || ['string', 'number', 'boolean'].includes(typeof v);
}

/** YAML source text for a scalar, in the quoting style of the old node. */
function scalarText(value: Primitive, type: string | undefined): string | null {
  if (typeof value !== 'string') return value === null ? 'null' : String(value);
  if (type === 'QUOTE_DOUBLE') return JSON.stringify(value);
  if (type === 'QUOTE_SINGLE') return `'${value.replace(/'/g, "''")}'`;
  if (type === 'BLOCK_LITERAL' || type === 'BLOCK_FOLDED') return null;
  if (value === '') return '""';
  const s = stringify(value, { lineWidth: 0 }).replace(/\n$/, '');
  return s.includes('\n') ? JSON.stringify(value) : s;
}

interface Style {
  padding: boolean;
  finalNewline: boolean;
}

function styleOf(content: string): Style {
  return {
    padding: /\{ \S/.test(content) || /\[ \S/.test(content),
    finalNewline: content === '' || content.endsWith('\n'),
  };
}

function render(doc: Document, st: Style): string {
  let out = doc.toString({ lineWidth: 0, flowCollectionPadding: st.padding });
  if (!st.finalNewline) out = out.replace(/\n$/, '');
  return out;
}

function load(content: string): Document {
  const doc = parseDocument(content, { prettyErrors: false });
  if (doc.errors.length) throw new Error(`the YAML has an error: ${doc.errors[0].message.split('\n')[0]}`);
  return doc;
}

/** Key order of the map at a path (schema order), for new keys. */
export type KeyOrder = (parent: Path) => string[] | undefined;

function keyOf(pair: { key: unknown }): unknown {
  return isScalar(pair.key) ? pair.key.value : pair.key;
}

/** Move key k of map m to its place in order (before the first later key). */
function placeKey(m: { items: { key: unknown }[] }, k: string, order: string[]) {
  const at = m.items.findIndex((p) => keyOf(p) === k);
  const rank = order.indexOf(k);
  if (at < 0 || rank < 0) return;
  const [pair] = m.items.splice(at, 1);
  const next = m.items.findIndex((p) => {
    const r = order.indexOf(String(keyOf(p)));
    return r > rank;
  });
  m.items.splice(next < 0 ? m.items.length : next, 0, pair);
}

// Parsed origins, so that each keystroke does not parse the file again.
let originCache: { text: string; doc: Document } | null = null;

/**
 * Quoting style for a scalar edit. With the text as loaded from disk
 * (origin), the style of the node there wins: a value that was plain stays
 * plain even when an intermediate keystroke needed quotes ("5" on the way
 * to "5m"). Without an origin, the style of the current node is kept.
 * An empty quoted placeholder ("" in a skeleton) takes the plain style.
 */
function styleFor(node: { type?: string; value: unknown }, path: Path, origin?: string): string | undefined {
  if (node.type === 'BLOCK_LITERAL' || node.type === 'BLOCK_FOLDED') return node.type;
  if (origin === undefined) return node.value === '' ? undefined : node.type;
  if (originCache?.text !== origin) originCache = { text: origin, doc: parseDocument(origin, { prettyErrors: false }) };
  let was: unknown;
  try {
    was = originCache.doc.getIn(path, true);
  } catch {
    was = undefined;
  }
  if (!isScalar(was) || was.value === '') return undefined;
  return was.type === 'QUOTE_DOUBLE' || was.type === 'QUOTE_SINGLE' ? was.type : undefined;
}

/**
 * Set a value at path. Returns the new text. A key that is not in the
 * document yet goes to its place in keyOrder (the schema order), not to
 * the end of its map. origin is the file as loaded from disk: scalars keep
 * the quoting style they have there (see styleFor).
 */
export function setIn(content: string, path: Path, value: unknown, keyOrder?: KeyOrder, origin?: string): string {
  const doc = load(content);
  const node = doc.getIn(path, true);
  if (isScalar(node) && node.range && isPrimitive(value)) {
    const [start, end] = node.range;
    const text = scalarText(value, styleFor(node, path, origin));
    if (text !== null && end > start) {
      return content.slice(0, start) + text + content.slice(end);
    }
  }
  const st = styleOf(content);
  // A new object in a list: flow style like its siblings (or compact when
  // the list was empty), and an empty flow list becomes a block list.
  const parentPath = path.slice(0, -1);
  const parent = parentPath.length ? doc.getIn(parentPath, true) : doc.contents;
  let created: unknown = value;
  if (isSeq(parent) && value !== null && typeof value === 'object') {
    const first = parent.items[0];
    const flow = isMap(first) || isSeq(first) ? Boolean((first as { flow?: boolean }).flow) : true;
    if (parent.flow) parent.flow = false;
    created = doc.createNode(value, { flow });
  } else if (value !== null && typeof value === 'object') {
    created = doc.createNode(value);
  }
  if (doc.contents === null) doc.contents = doc.createNode({});
  // The first level of the path that is new (its parent map exists).
  let fresh = -1;
  if (keyOrder) for (let i = 0; i < path.length; i++) if (!doc.hasIn(path.slice(0, i + 1))) { fresh = i; break; }
  doc.setIn(path, created);
  if (fresh >= 0 && typeof path[fresh] === 'string') {
    const pp = path.slice(0, fresh);
    const m = pp.length ? doc.getIn(pp, true) : doc.contents;
    const order = keyOrder!(pp);
    if (isMap(m) && order) placeKey(m as unknown as { items: { key: unknown }[] }, path[fresh] as string, order);
  }
  return render(doc, st);
}

/** Delete the value at path (a map key or a list item). */
export function deleteIn(content: string, path: Path): string {
  const doc = load(content);
  if (!doc.hasIn(path)) return content;
  const st = styleOf(content);
  doc.deleteIn(path);
  return render(doc, st);
}

/** Append an item to the list at path (made when missing). */
export function appendIn(content: string, path: Path, value: unknown, keyOrder?: KeyOrder): string {
  const doc = load(content);
  const seq = doc.getIn(path, true);
  const n = isSeq(seq) ? seq.items.length : 0;
  if (!isSeq(seq)) {
    // Missing or null: make an empty list first (at its schema place).
    if (doc.hasIn(path)) doc.deleteIn(path);
    return setIn(setIn(render(doc, styleOf(content)), path, [], keyOrder), [...path, 0], value);
  }
  return setIn(content, [...path, n], value);
}

/** Value at path in plain data. */
export function getAt(data: unknown, path: Path): unknown {
  let cur: unknown = data;
  for (const p of path) {
    if (cur === null || typeof cur !== 'object') return undefined;
    cur = (cur as Record<string | number, unknown>)[p];
  }
  return cur;
}
