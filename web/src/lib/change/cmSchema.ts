// CodeMirror extensions for YAML with a kind schema: completion of keys and
// values, hover with title, description, truth class and owner, protection
// of masked secret values, and API problems as diagnostics.
import type { Completion, CompletionContext, CompletionResult } from '@codemirror/autocomplete';
import { EditorState, type Extension, type Text } from '@codemirror/state';
import { Decoration, EditorView, hoverTooltip, MatchDecorator, ViewPlugin, type DecorationSet, type ViewUpdate } from '@codemirror/view';
import type { Diagnostic } from '@codemirror/lint';
import type { Problem } from '../api/types';
import { contextAt, pathAtLine } from './yamlPath';
import { at, enumOf, fields, labelOf, typeOf, ui, type JS } from './schema';
import { formatPath } from './yamlEdit';

export const MASK = '••••••••';

/** Completion from a schema getter (the schema can load later). */
export function schemaCompletion(getSchema: () => JS | null) {
  return (ctx: CompletionContext): CompletionResult | null => {
    const root = getSchema();
    if (!root) return null;
    const text = ctx.state.doc.toString();
    const c = contextAt(text, ctx.pos);
    if (!ctx.explicit && c.prefix === '' && !c.inValue) return null;
    if (c.inValue && c.key) {
      const s = at(root, [...c.parent, c.key]);
      const opts: Completion[] = [];
      for (const v of enumOf(s) ?? []) opts.push({ label: String(v), type: 'enum' });
      if (typeOf(s) === 'boolean') opts.push({ label: 'true', type: 'keyword' }, { label: 'false', type: 'keyword' });
      if (ui(s).widget === 'duration') {
        for (const d of ['30s', '1m', '5m', '10m', '1h']) opts.push({ label: d, type: 'constant', detail: 'duration' });
      }
      if (!opts.length) return null;
      return { from: c.from, options: opts, validFor: /^[\w./:-]*$/ };
    }
    const parent = at(root, c.parent);
    if (!parent) return null;
    const present = presentKeys(text, ctx.pos);
    const options: Completion[] = fields(root, parent)
      .filter((f) => !present.has(f.key))
      .map((f) => {
        const t = typeOf(f.schema);
        const snippetTail = t === 'object' ? ':\n' : t === 'array' ? ':\n  - ' : ': ';
        return {
          label: f.key,
          type: 'property',
          detail: [t, f.required ? 'required' : ''].filter(Boolean).join(' · '),
          info: describe(f.key, f.schema) || undefined,
          apply: f.key + snippetTail,
          boost: f.required ? 2 : 0,
        };
      });
    return { from: c.from, options, validFor: /^[\w.-]*$/ };
  };
}

/** Keys already present in the map at the cursor's indentation. */
function presentKeys(text: string, pos: number): Set<string> {
  const lines = text.split('\n');
  const lineNo = text.slice(0, pos).split('\n').length - 1;
  const cur = lines[lineNo] ?? '';
  const indent = cur.length - cur.trimStart().replace(/^-\s+/, '').length;
  const out = new Set<string>();
  const re = /^(\s*(?:-\s+)?)([\w.-]+):/;
  for (const dir of [-1, 1]) {
    for (let i = lineNo + dir; i >= 0 && i < lines.length; i += dir) {
      const l = lines[i];
      if (!l.trim() || l.trimStart().startsWith('#')) continue;
      const m = re.exec(l);
      const ind = l.length - l.trimStart().length;
      if (m && m[1].length === indent) out.add(m[2]);
      else if (ind < indent) break;
      if (m && m[1].includes('-') && dir === -1) break;
    }
  }
  return out;
}

/** One-paragraph description of a field from the schema. */
export function describe(key: string, s: JS | undefined): string {
  if (!s) return '';
  const parts: string[] = [];
  const label = labelOf(key, s);
  parts.push(label);
  if (s.description && s.description !== label) parts.push(s.description);
  const meta: string[] = [];
  if (s['x-pk-truth']) meta.push(`truth: ${s['x-pk-truth']}`);
  if (s['x-pk-owner']) meta.push(`owner: ${s['x-pk-owner']}`);
  if (s['x-pk-ref']?.kind) meta.push(`ref: ${s['x-pk-ref'].kind}`);
  if (s['x-pk-secret']) meta.push('secret');
  const e = enumOf(s);
  if (e) meta.push(`one of: ${e.join(', ')}`);
  if (s['x-pk-message']) meta.push(s['x-pk-message']);
  else if (s.pattern) meta.push(`pattern ${s.pattern}`);
  if (meta.length) parts.push(meta.join(' · '));
  return parts.join('\n');
}

/** Hover on a key: schema title, description, truth class, owner. */
export function schemaHover(getSchema: () => JS | null): Extension {
  return hoverTooltip((view, pos) => {
    const root = getSchema();
    if (!root) return null;
    const line = view.state.doc.lineAt(pos);
    const m = /^(\s*(?:-\s+)*)([\w.-]+):/.exec(line.text);
    if (!m) return null;
    const start = line.from + m[1].length;
    const end = start + m[2].length;
    if (pos < start || pos > end + 1) return null;
    const path = pathAtLine(view.state.doc.toString(), line.number);
    if (!path) return null;
    const s = at(root, path);
    if (!s) return null;
    return {
      pos: start,
      end,
      above: true,
      create() {
        const dom = document.createElement('div');
        dom.className = 'cm-pk-hover';
        const [head, ...rest] = describe(String(path[path.length - 1]), s).split('\n');
        const h = document.createElement('b');
        h.textContent = head;
        const p = document.createElement('code');
        p.textContent = formatPath(path);
        dom.append(h, document.createTextNode(' '), p);
        for (const r of rest) {
          const d = document.createElement('div');
          d.textContent = r;
          dom.append(d);
        }
        return { dom };
      },
    };
  });
}

function maskRanges(doc: Text): number[] {
  const out: number[] = [];
  const text = doc.toString();
  let i = text.indexOf(MASK);
  while (i >= 0) {
    out.push(i, i + MASK.length);
    i = text.indexOf(MASK, i + MASK.length);
  }
  return out;
}

const maskDeco = new MatchDecorator({
  regexp: /••••••••/g,
  decoration: () => Decoration.mark({ class: 'cm-pk-secret', attributes: { title: 'secret value, masked (SOPS); it keeps its stored value on save' } }),
});

/** Masked secrets are shown with a lock style and cannot be edited. */
export function protectSecrets(): Extension {
  return [
    EditorState.changeFilter.of((tr) => {
      const r = maskRanges(tr.startState.doc);
      return r.length ? r : true;
    }),
    ViewPlugin.fromClass(
      class {
        decorations: DecorationSet;
        constructor(view: EditorView) {
          this.decorations = maskDeco.createDeco(view);
        }
        update(u: ViewUpdate) {
          this.decorations = maskDeco.updateDeco(u, this.decorations);
        }
      },
      { decorations: (v) => v.decorations },
    ),
  ];
}

/** API problems (1-based line and col) as CodeMirror diagnostics. */
export function toDiagnostics(doc: Text, problems: Problem[]): Diagnostic[] {
  const out: Diagnostic[] = [];
  for (const p of problems) {
    const ln = Math.min(Math.max(p.line ?? 1, 1), doc.lines);
    const line = doc.line(ln);
    let from = line.from;
    if (p.col && p.col > 0) from = Math.min(line.from + p.col - 1, line.to);
    else from = line.from + (line.text.length - line.text.trimStart().length);
    let to = line.to;
    if (from >= to) to = Math.min(from + 1, doc.length);
    out.push({
      from,
      to: Math.max(to, from),
      severity: p.severity === 'error' ? 'error' : p.severity === 'warning' ? 'warning' : 'info',
      message: p.message,
      source: p.source,
    });
  }
  return out;
}
