// Which fields the secret filter removed from a prompt: the YAML key path
// of each "[redacted]" value, read from the indentation of the context
// block it is in.

export interface RedactedField {
  /** Context file of the value, from the "--- context: <path> ..." line. */
  file?: string;
  /** Key path, for example hubble.ui.auth.token (spec.values. dropped). */
  field: string;
}

const KEY = /^(\s*)(?:-\s+)?([A-Za-z0-9_.\/-]+):(?:\s|$)/;

export function redactedFields(prompt: string): RedactedField[] {
  const out: RedactedField[] = [];
  let file: string | undefined;
  let stack: { indent: number; key: string }[] = [];
  for (const line of prompt.split('\n')) {
    const ctx = line.match(/^--- context: (\S+)/);
    if (ctx || line.startsWith('---')) {
      file = ctx?.[1];
      stack = [];
      continue;
    }
    const m = line.match(KEY);
    if (!m) {
      if (line.includes('[redacted]')) out.push({ file, field: '(value)' });
      continue;
    }
    // A list item ("- key:") sits one level deeper than its dash.
    const indent = m[1].length + (line.slice(m[1].length).startsWith('-') ? 2 : 0);
    while (stack.length && stack[stack.length - 1].indent >= indent) stack.pop();
    stack.push({ indent, key: m[2] });
    if (line.includes('[redacted]')) {
      out.push({ file, field: stack.map((s) => s.key).join('.').replace(/^spec\.values\./, '') });
    }
  }
  return out;
}
