// Form model from a JSON Schema with x-pk hints and the parsed document:
// sections ("Intent", "Ports and cables", "Metadata", "Spec") of rows. Each
// row edits one value by path. Pure, tested with node --test.
import { enumOf, fields, isSecret, labelOf, refKind, typeOf, ui, type JS } from './schema.ts';
import { getAt, type Path } from './yamlEdit.ts';

export const MASKED = '••••••••';

export type RowKind =
  | 'text'
  | 'number'
  | 'boolean'
  | 'select'
  | 'duration'
  | 'secret'
  | 'rack'
  | 'port'
  | 'list'
  | 'tags'
  | 'map'
  | 'yaml';

export interface Row {
  kind: RowKind;
  path: Path;
  key: string;
  label: string;
  schema: JS;
  value: unknown;
  required: boolean;
  options?: unknown[];
  refKind?: string;
  truth?: string;
  owner?: string;
  readOnly: boolean;
  help?: string;
  unit?: string;
}

export interface Section {
  title: string;
  rows: Row[];
}

const SKIP_TOP = new Set(['apiVersion', 'kind', 'status', 'sops']);

function rowKind(s: JS, value: unknown): RowKind | 'object' {
  const w = ui(s).widget;
  if (isSecret(s) || value === MASKED) return 'secret';
  if (w === 'rack-picker') return 'rack';
  if (w === 'port-picker') return 'port';
  if (w === 'duration') return 'duration';
  const t = typeOf(s);
  if (w === 'select' || enumOf(s) || (refKind(s) && t === 'string')) return 'select';
  if (t === 'boolean') return 'boolean';
  if (t === 'integer' || t === 'number') return 'number';
  if (t === 'array') {
    const it = s.items ?? {};
    return typeOf(it) === 'object' || it.properties ? 'list' : 'tags';
  }
  if (t === 'object') {
    if (!s.properties && s.additionalProperties && typeof s.additionalProperties === 'object' && typeOf(s.additionalProperties) === 'string') return 'map';
    if (!s.properties) return 'yaml';
    return 'object';
  }
  return 'text';
}

function present(v: unknown): boolean {
  return v !== undefined;
}

/**
 * Rows for the properties of an object schema at path. Nested objects
 * without a widget are flattened (their path shows under the label).
 */
export function rowsOf(root: JS, s: JS, path: Path, data: unknown, depth = 0): { row: Row; group?: string }[] {
  const out: { row: Row; group?: string }[] = [];
  if (depth > 8) return out;
  for (const f of fields(root, s)) {
    if (depth === 0 && path.length === 0 && SKIP_TOP.has(f.key)) continue;
    const p = [...path, f.key];
    const v = getAt(data, p);
    const hinted = Object.keys(ui(f.schema)).length > 0 || !!f.schema['x-pk-secret'];
    const k = rowKind(f.schema, v);
    const group = ui(f.schema).group;
    if (k === 'object') {
      // Optional objects that are not in the document and carry no hint
      // below them stay out of the form (edit them in YAML).
      if (!present(v) && !f.required && !hasHints(root, f.schema)) continue;
      for (const r of rowsOf(root, f.schema, p, data, depth + 1)) out.push({ row: r.row, group: r.group ?? group });
      continue;
    }
    if (!present(v) && !f.required && !hinted) continue;
    const truth = f.schema['x-pk-truth'] as string | undefined;
    out.push({
      group,
      row: {
        kind: k,
        path: p,
        key: f.key,
        label: labelOf(f.key, f.schema),
        schema: f.schema,
        value: v,
        required: f.required,
        options: enumOf(f.schema),
        refKind: refKind(f.schema),
        truth,
        owner: f.schema['x-pk-owner'],
        readOnly: k === 'secret' || !!f.schema.readOnly || truth === 'derived' || truth === 'generated',
        help: ui(f.schema).help ?? f.schema.description,
        unit: ui(f.schema).unit,
      },
    });
  }
  return out;
}

function hasHints(root: JS, s: JS, depth = 0): boolean {
  if (depth > 6) return false;
  for (const f of fields(root, s)) {
    if (f.schema['x-pk-ui'] || f.schema['x-pk-secret']) return true;
    if (typeOf(f.schema) === 'object' && hasHints(root, f.schema, depth + 1)) return true;
  }
  return false;
}

/** The form: sections in order of first appearance. */
export function buildForm(root: JS, data: unknown): Section[] {
  const sections: Section[] = [];
  const byTitle = new Map<string, Section>();
  for (const f of fields(root, root)) {
    if (SKIP_TOP.has(f.key)) continue;
    const top = labelOf(f.key, undefined);
    const rows = rowsOf(root, { properties: { [f.key]: f.schema }, required: f.required ? [f.key] : [] }, [], data, 1);
    for (const { row, group } of rows) {
      const title = group ?? (top === 'Metadata' || top === 'Spec' ? top : labelOf(f.key, f.schema));
      let sec = byTitle.get(title);
      if (!sec) {
        sec = { title, rows: [] };
        byTitle.set(title, sec);
        sections.push(sec);
      }
      sec.rows.push(row);
    }
  }
  // Metadata first, as in kubectl output and the mocks.
  const m = sections.findIndex((x) => x.title === 'Metadata');
  if (m > 0) sections.unshift(...sections.splice(m, 1));
  return sections;
}

/** Problems of a row: same field, or a field below it (objects, lists). */
export function problemsAt<T extends { field?: string }>(problems: T[], field: string): T[] {
  return problems.filter((p) => p.field === field || (p.field ?? '').startsWith(field + '.') || (p.field ?? '').startsWith(field + '['));
}

/** Next port name for a node: bmc first, then p1, p2, ... */
export function nextPortName(items: unknown[]): string {
  const names = new Set(items.map((i) => (i && typeof i === 'object' ? String((i as JS).name ?? '') : '')));
  if (!names.has('bmc')) return 'bmc';
  for (let n = 1; ; n++) if (!names.has(`p${n}`)) return `p${n}`;
}
