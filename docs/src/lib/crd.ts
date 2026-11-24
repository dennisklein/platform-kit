// Reads a CRD from metal/config/crd and flattens its schema into table rows.
// The rows are the same that the generated JSON of metal/hack/docgen holds; when that
// data exists, FieldTable can read it instead.
import { readFileSync, readdirSync } from 'node:fs';
import { root } from '../../scripts/root.mjs';
import { parse } from 'yaml';

const crdDir = `${root}/../metal/config/crd/`;

export interface FieldRow {
  path: string;
  depth: number;
  type: string;
  required: boolean;
  enum?: string[];
  default?: unknown;
  description?: string;
}

export interface Crd {
  kind: string;
  group: string;
  version: string;
  plural: string;
  scope: string;
  file: string;
  spec: FieldRow[];
  status: FieldRow[];
}

/** Types whose fields every Kubernetes user knows: one row, no children. */
const opaque = new Set(['conditions']);

function typeOf(s: any): string {
  if (s.type === 'array') return `[]${typeOf(s.items ?? {})}`;
  if (s.format) return `${s.type} (${s.format})`;
  if (s['x-kubernetes-int-or-string']) return 'int or string';
  return s.type ?? 'any';
}

function flatten(s: any, prefix: string, depth: number, required: string[], out: FieldRow[]) {
  for (const [name, f] of Object.entries<any>(s.properties ?? {})) {
    const path = prefix ? `${prefix}.${name}` : name;
    out.push({
      path, depth, type: typeOf(f), required: required.includes(name),
      enum: f.enum ?? f.items?.enum, default: f.default, description: f.description,
    });
    if (opaque.has(name)) continue;
    const inner = f.type === 'array' ? f.items : f;
    if (inner?.properties) flatten(inner, f.type === 'array' ? `${path}[]` : path, depth + 1, inner.required ?? [], out);
  }
}

const cache = new Map<string, Crd>();

export function loadCrd(kind: string): Crd {
  const hit = cache.get(kind);
  if (hit) return hit;
  for (const file of readdirSync(crdDir)) {
    if (!file.endsWith('.yaml') || file === 'kustomization.yaml') continue;
    const doc = parse(readFileSync(crdDir + file, 'utf8'));
    if (doc?.spec?.names?.kind !== kind) continue;
    const v = doc.spec.versions.find((x: any) => x.storage) ?? doc.spec.versions[0];
    const props = v.schema.openAPIV3Schema.properties;
    const crd: Crd = {
      kind, group: doc.spec.group, version: v.name, plural: doc.spec.names.plural,
      scope: doc.spec.scope, file: `metal/config/crd/${file}`, spec: [], status: [],
    };
    for (const part of ['spec', 'status'] as const) {
      if (props[part]) flatten(props[part], part, 0, props[part].required ?? [], crd[part]);
    }
    cache.set(kind, crd);
    return crd;
  }
  throw new Error(`crd: no CRD for kind "${kind}" in metal/config/crd`);
}
