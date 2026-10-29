// Where an opened LLM log entry scrolls to. Only the prompt box scrolls to
// the removed values; the page scrolls just enough to show the entry from
// its header, so the red "values removed" box and the answer stay on screen.

export interface Span {
  /** Offset of the mark from the top of the prompt box's content. */
  top: number;
  height: number;
}

/** scrollTop of the prompt box that shows the removed values: all of them
 *  centred when they fit, else the first one a third down the box. */
export function promptScrollTop(marks: Span[], boxHeight: number, scrollHeight: number): number {
  if (!marks.length || boxHeight <= 0) return 0;
  const first = marks[0].top;
  const last = marks[marks.length - 1];
  const end = last.top + last.height;
  const top = end - first <= boxHeight * 0.8 ? (first + end) / 2 - boxHeight / 2 : first - boxHeight / 3;
  const max = Math.max(0, scrollHeight - boxHeight);
  return Math.round(Math.min(max, Math.max(0, top)));
}

/** True when the page has to scroll to show the entry: its header is above
 *  the visible area or its end is below it. */
export function entryNeedsScroll(entry: { top: number; bottom: number }, view: { top: number; bottom: number }): boolean {
  return entry.top < view.top || entry.bottom > view.bottom;
}
