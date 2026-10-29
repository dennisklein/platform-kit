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

/** The MR description: the bench's when there is an MR, else a preview. */
export function descriptionPreview(c: Change): string {
  if (c.description) return c.description;
  const lines = [c.title, ''];
  if (c.intent) lines.push(c.intent, '');
  for (const [k, v] of trailerLines(c)) lines.push(`${k}: ${v}`);
  return lines.join('\n');
}

/** The merge commit message the bench writes. */
export function mergePreview(c: Change): string {
  const mr = c.mr ? ` (!${c.mr.number})` : '';
  const lines = [`Merge change ${c.id}: ${c.title}${mr}`, ''];
  if (c.intent) lines.push(c.intent, '');
  for (const [k, v] of trailerLines(c)) if (k !== 'Pk-Title') lines.push(`${k}: ${v}`);
  const by = (c.approvalsBy ?? []).filter((a) => a.counts).map((a) => a.user);
  lines.push(`Approved-by: ${by.length ? by.join(', ') : '(none yet)'}`);
  if (c.mr) lines.push(`Reviewed-in: !${c.mr.number}`);
  return lines.join('\n');
}
