// Reads a kind's fields from the generated CRD data (src/data/generated/crds, written
// by metal/hack/docgen from metal/config/crd) and shapes them into table rows.
// docgen has already flattened the schema and rewritten the text that the site must
// not show as is (the upstream text of object reference fields).
import { readFileSync, readdirSync } from 'node:fs';
import { root } from '../../scripts/root.mjs';

const dataDir = `${root}/src/data/generated/crds/`;

export interface FieldRow {
  path: string;
  depth: number;
  type: string;
  required: boolean;
  enum?: string[];
  default?: unknown;
  description?: string;
  /** Bounds in words: "0 to 3600", "at most 63 characters", "1 to 3 items". */
  bounds?: string;
  pattern?: string;
  /** The messages of the CEL rules (x-kubernetes-validations) on the field. */
  rules?: string[];
}

/** A column of `kubectl get`; priority 1 shows only with `-o wide`. */
export interface Column {
  name: string;
  jsonPath: string;
  type: string;
  wide: boolean;
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
  /** CEL rule messages on the spec or status object itself. */
  rules: { spec: string[]; status: string[] };
  columns: Column[];
}

/** Fields whose inner fields every Kubernetes user knows: one row, no children. */
const opaque = ['status.conditions'];

const range = (lo: number | undefined, hi: number | undefined, unit = '') => {
  const u = unit ? ` ${unit}` : '';
  if (lo !== undefined && hi !== undefined) return lo === hi ? `exactly ${lo}${u}` : `${lo} to ${hi}${u}`;
  if (hi !== undefined) return `at most ${hi}${u}`;
  if (lo !== undefined) return `at least ${lo}${u}`;
  return undefined;
};

/** The bounds of a field, in words. */
function boundsOf(f: any): string | undefined {
  const parts = [
    range(f.minimum, f.maximum),
    range(f.minLength, f.maxLength, 'characters'),
    range(f.minItems, f.maxItems, 'items'),
    range(f.minProperties, f.maxProperties, 'entries'),
  ].filter(Boolean);
  return parts.length ? parts.join(', ') : undefined;
}

const messages = (vs: any[] | undefined): string[] => (vs ?? []).map((v) => v.message || v.rule);

function rowOf(f: any): FieldRow {
  const rules = messages(f.validations);
  return {
    path: f.path,
    depth: f.depth - 1,
    type: f.format && !f.type.startsWith('[]') ? `${f.type} (${f.format})` : f.type,
    required: f.required,
    enum: f.enum,
    default: f.default,
    description: f.description || undefined,
    bounds: boundsOf(f),
    pattern: f.pattern,
    rules: rules.length ? rules : undefined,
  };
}

const cache = new Map<string, Crd>();

export function loadCrd(kind: string): Crd {
  const hit = cache.get(kind);
  if (hit) return hit;
  for (const file of readdirSync(dataDir)) {
    if (!file.endsWith('.json')) continue;
    const doc = JSON.parse(readFileSync(dataDir + file, 'utf8'));
    if (doc.kind !== kind) continue;
    const fields: any[] = doc.fields;
    const part = (p: 'spec' | 'status') =>
      fields
        .filter((f) => f.path.startsWith(`${p}.`) && !opaque.some((o) => f.path.startsWith(`${o}.`) || f.path.startsWith(`${o}[]`)))
        .map(rowOf);
    const top = (p: string) => messages(fields.find((f) => f.path === p)?.validations);
    const crd: Crd = {
      kind, group: doc.group, version: doc.version, plural: doc.plural, scope: doc.scope, file: doc.source,
      spec: part('spec'),
      status: part('status'),
      rules: { spec: [...messages(doc.validations), ...top('spec')], status: top('status') },
      columns: doc.printerColumns.map((c: any) => ({
        name: c.name, jsonPath: c.jsonPath, type: c.type, wide: c.priority > 0, description: c.description,
      })),
    };
    cache.set(kind, crd);
    return crd;
  }
  throw new Error(`crd: no generated data for kind "${kind}" in src/data/generated/crds (run mise run docs:gen)`);
}
