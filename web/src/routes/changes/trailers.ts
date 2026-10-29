// MR description and merge commit previews from the change metadata (the
// bench writes the real ones; these show what will be written).
import type { Change } from '../../lib/api/types';

function latestPass(c: Change) {
  const runs = c.labRuns ?? c.meta?.labRuns ?? [];
  return [...runs].reverse().find((r) => r.result === 'pass');
}

/** Trailer block from the change metadata. */
export function trailerLines(c: Change): [string, string][] {
  const t: [string, string][] = [['Pk-Change', c.id]];
  t.push(['Pk-Title', c.title]);
  if (c.intent) t.push(['Pk-Intent', c.intent]);
  const run = latestPass(c);
  if (run) t.push(['Pk-Lab-Run', `${run.run} ${run.result}${run.report ? ' ' + run.report : ''}`]);
  t.push(['Pk-Author', c.author]);
  if (c.agent) t.push(['Pk-Agent', c.agent]);
  for (const w of c.workItems ?? []) t.push(['Pk-Work-Item', w]);
  return t;
}

/** A commit message or MR description in parts: subject, body, trailers. */
export interface MessageParts {
  subject: string;
  body: string[];
  trailers: [string, string][];
}

const TRAILER = /^([A-Z][A-Za-z0-9-]*):\s?(.*)$/;

/**
 * Split a message: the first paragraph is the subject, the last paragraph
 * is the trailer block when every line of it is "Key: value".
 */
export function parseMessage(text: string): MessageParts {
  const paras = text
    .replace(/\r\n/g, '\n')
    .trim()
    .split(/\n\s*\n/)
    .map((p) => p.trim())
    .filter(Boolean);
  let trailers: [string, string][] = [];
  const last = paras[paras.length - 1];
  if (last && paras.length > 1) {
    const lines = last.split('\n');
    if (lines.every((l) => TRAILER.test(l))) {
      trailers = lines.map((l) => {
        const m = l.match(TRAILER)!;
        return [m[1], m[2]];
      });
      paras.pop();
    }
  }
  return { subject: paras[0] ?? '', body: paras.slice(1), trailers };
}

/** The MR description in parts: the bench's when there is an MR, else a preview. */
export function descriptionParts(c: Change): MessageParts {
  if (c.description) {
    const p = parseMessage(c.description);
    // The bench's description starts with the intent, not a subject.
    return { subject: '', body: [p.subject, ...p.body].filter(Boolean), trailers: p.trailers };
  }
  return { subject: c.title, body: c.intent ? [c.intent] : [], trailers: trailerLines(c) };
}

/** The MR description: the bench's when there is an MR, else a preview. */
export function descriptionPreview(c: Change): string {
  if (c.description) return c.description;
  const lines = [c.title, ''];
  if (c.intent) lines.push(c.intent, '');
  for (const [k, v] of trailerLines(c)) lines.push(`${k}: ${v}`);
  return lines.join('\n');
}

/** The merge commit message the bench writes, in parts. */
export function mergeParts(c: Change): MessageParts {
  const mr = c.mr ? ` (!${c.mr.number})` : '';
  const trailers = trailerLines(c).filter(([k]) => k !== 'Pk-Title');
  const by = (c.approvalsBy ?? []).filter((a) => a.counts).map((a) => a.user);
  trailers.push(['Approved-by', by.length ? by.join(', ') : '(none yet)']);
  if (c.mr) trailers.push(['Reviewed-in', `!${c.mr.number}`]);
  return { subject: `Merge change ${c.id}: ${c.title}${mr}`, body: c.intent ? [c.intent] : [], trailers };
}

/** The merge commit message the bench writes. */
export function mergePreview(c: Change): string {
  const p = mergeParts(c);
  const lines = [p.subject, ''];
  for (const b of p.body) lines.push(b, '');
  for (const [k, v] of p.trailers) lines.push(`${k}: ${v}`);
  return lines.join('\n');
}
