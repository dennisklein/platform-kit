// JSON Schema helpers for the form and editor (with x-pk extensions).
import type { Path } from './yamlEdit';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
export type JS = Record<string, any>;

export interface UiHints {
  widget?: 'select' | 'rack-picker' | 'port-picker' | 'duration' | string;
  label?: string;
  help?: string;
  order?: number;
  group?: string;
  unit?: string;
}

/** Follow local $ref (#/$defs/..., #/definitions/...) and merge allOf. */
export function resolve(root: JS, s: JS | undefined, depth = 0): JS | undefined {
  if (!s || depth > 20) return s;
  let out = s;
  if (typeof s.$ref === 'string' && s.$ref.startsWith('#/')) {
    let t: JS | undefined = root;
    for (const part of s.$ref.slice(2).split('/')) t = t?.[part.replace(/~1/g, '/').replace(/~0/g, '~')];
    const { $ref: _ref, ...rest } = s;
    out = { ...(resolve(root, t, depth + 1) ?? {}), ...rest };
  }
  if (Array.isArray(out.allOf)) {
    const merged: JS = { ...out };
    delete merged.allOf;
    for (const part of out.allOf) {
      const r = resolve(root, part, depth + 1) ?? {};
      merged.properties = { ...(merged.properties ?? {}), ...(r.properties ?? {}) };
      merged.required = [...(merged.required ?? []), ...(r.required ?? [])];
      for (const [k, v] of Object.entries(r)) if (!(k in merged)) merged[k] = v;
    }
    out = merged;
  }
  return out;
}

/** Sub-schema at a data path (properties, items, additionalProperties). */
export function at(root: JS, path: Path): JS | undefined {
  let s = resolve(root, root);
  for (const p of path) {
    if (!s) return undefined;
    if (typeof p === 'number') {
      s = resolve(root, Array.isArray(s.items) ? s.items[p] : s.items);
    } else if (s.properties && p in s.properties) {
      s = resolve(root, s.properties[p]);
    } else if (s.additionalProperties && typeof s.additionalProperties === 'object') {
      s = resolve(root, s.additionalProperties);
    } else if (s.type === 'array' || s.items) {
      s = resolve(root, s.items);
      if (s?.properties && p in s.properties) s = resolve(root, s.properties[p]);
      else return undefined;
    } else return undefined;
  }
  return s;
}

export function ui(s: JS | undefined): UiHints {
  return (s?.['x-pk-ui'] as UiHints) ?? {};
}

export function typeOf(s: JS | undefined): string {
  if (!s) return 'any';
  if (Array.isArray(s.type)) return s.type.find((t: string) => t !== 'null') ?? 'any';
  if (s.type) return s.type;
  if (s.properties) return 'object';
  if (s.items) return 'array';
  if (s.enum) return typeof s.enum[0];
  return 'any';
}

export function enumOf(s: JS | undefined): unknown[] | undefined {
  if (!s) return undefined;
  if (Array.isArray(s.enum)) return s.enum;
  if (s.const !== undefined) return [s.const];
  return undefined;
}

export function isSecret(s: JS | undefined): boolean {
  return Boolean(s?.['x-pk-secret']);
}

export function refKind(s: JS | undefined): string | undefined {
  return s?.['x-pk-ref']?.kind;
}

/** Label from x-pk-ui.label, title or the key ("bgpControlPlane" → "Bgp control plane"). */
export function labelOf(key: string | number, s: JS | undefined): string {
  const l = ui(s).label ?? s?.title;
  if (l) return l;
  const k = String(key)
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .replace(/[_-]+/g, ' ')
    .toLowerCase();
  return k.charAt(0).toUpperCase() + k.slice(1);
}

export interface Field {
  key: string;
  schema: JS;
  required: boolean;
}

/** Properties of an object schema, sorted by x-pk-ui.order, then schema order. */
export function fields(root: JS, s: JS | undefined): Field[] {
  const r = resolve(root, s);
  if (!r?.properties) return [];
  const req = new Set<string>(r.required ?? []);
  const keys = Object.keys(r.properties);
  return keys
    .map((key, i) => ({ key, schema: resolve(root, r.properties[key]) ?? {}, required: req.has(key), i }))
    .sort((a, b) => (ui(a.schema).order ?? 1000 + a.i) - (ui(b.schema).order ?? 1000 + b.i))
    .map(({ key, schema, required }) => ({ key, schema, required }));
}

/** A fresh value for a schema (defaults, required members), for new list items. */
export function skeleton(root: JS, s: JS | undefined, depth = 0): unknown {
  const r = resolve(root, s);
  if (!r || depth > 6) return null;
  if (r.default !== undefined) return structuredClone(r.default);
  const e = enumOf(r);
  switch (typeOf(r)) {
    case 'object': {
      const o: JS = {};
      for (const f of fields(root, r)) if (f.required) o[f.key] = skeleton(root, f.schema, depth + 1);
      return o;
    }
    case 'array':
      return [];
    case 'integer':
    case 'number':
      return r.minimum ?? 0;
    case 'boolean':
      return false;
    case 'string':
      return e ? e[0] : '';
    default:
      return e ? e[0] : null;
  }
}

/** Check a scalar against the schema (type, enum, pattern, min/max). */
export function checkValue(s: JS | undefined, v: unknown): string | null {
  if (!s || v === undefined || v === null || v === '') return null;
  const t = typeOf(s);
  if ((t === 'integer' || t === 'number') && typeof v === 'number') {
    if (t === 'integer' && !Number.isInteger(v)) return 'must be a whole number';
    if (typeof s.minimum === 'number' && v < s.minimum) return `must be at least ${s.minimum}`;
    if (typeof s.maximum === 'number' && v > s.maximum) return `must be at most ${s.maximum}`;
  }
  if (typeof v === 'string' && typeof s.pattern === 'string') {
    try {
      if (!new RegExp(s.pattern).test(v)) return s['x-pk-message'] ?? `must match ${s.pattern}`;
    } catch {
      // invalid pattern in schema: skip
    }
  }
  const e = enumOf(s);
  if (e && !e.includes(v)) return `must be one of ${e.join(', ')}`;
  return null;
}
