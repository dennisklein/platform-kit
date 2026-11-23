// Field-level compare of two documents (dotted paths), for "Compare with
// now" in time travel: formatting and key order do not matter.
// No imports: unit tested with node --test (web/unit).

export type Flat = Map<string, string>;

const scalar = (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v));

/** Flatten a parsed document: {spec: {ports: [{name: p1}]}} -> spec.ports[0].name = p1. */
export function flatten(v: unknown, prefix = '', out: Flat = new Map()): Flat {
  if (Array.isArray(v)) {
    if (!v.length) out.set(prefix, '[]');
    v.forEach((x, i) => flatten(x, `${prefix}[${i}]`, out));
  } else if (v && typeof v === 'object') {
    const keys = Object.keys(v as Record<string, unknown>);
    if (!keys.length && prefix) out.set(prefix, '{}');
    for (const k of keys) flatten((v as Record<string, unknown>)[k], prefix ? `${prefix}.${k}` : k, out);
  } else if (prefix) {
    out.set(prefix, v === null || v === undefined ? 'null' : scalar(v));
  }
  return out;
}

export interface FieldChange {
  field: string;
  op: 'add' | 'change' | 'delete';
  before?: string;
  after?: string;
}

/** Changed fields from a to b, in the order of b (deleted fields last). */
export function compareFields(a: unknown, b: unknown): FieldChange[] {
  const fa = flatten(a);
  const fb = flatten(b);
  const out: FieldChange[] = [];
  for (const [k, v] of fb) {
    if (!fa.has(k)) out.push({ field: k, op: 'add', after: v });
    else if (fa.get(k) !== v) out.push({ field: k, op: 'change', before: fa.get(k), after: v });
  }
  for (const [k, v] of fa) if (!fb.has(k)) out.push({ field: k, op: 'delete', before: v });
  return out;
}
