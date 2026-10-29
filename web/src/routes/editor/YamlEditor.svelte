<script lang="ts">
  // CodeMirror 6 for YAML (and Markdown, text): schema completion and hover,
  // API problems as diagnostics, masked secrets protected. Loaded lazily.
  import { onMount } from 'svelte';
  import { basicSetup } from 'codemirror';
  import { EditorView, keymap } from '@codemirror/view';
  import { Compartment, EditorState, EditorSelection } from '@codemirror/state';
  import { yaml } from '@codemirror/lang-yaml';
  import { autocompletion } from '@codemirror/autocomplete';
  import { HighlightStyle, syntaxHighlighting } from '@codemirror/language';
  import { tags as t } from '@lezer/highlight';
  import { lintGutter, setDiagnostics } from '@codemirror/lint';
  import type { Problem } from '../../lib/api/types';
  import type { JS } from '../../lib/change/schema';
  import { protectSecrets, schemaCompletion, schemaHover, toDiagnostics } from '../../lib/change/cmSchema';

  let {
    value = $bindable(''),
    language = 'yaml',
    readOnly = false,
    problems = [],
    schema = null,
    line = 0,
    onsave,
    oncursor,
  }: {
    value?: string;
    language?: string;
    readOnly?: boolean;
    problems?: Problem[];
    schema?: JS | null;
    line?: number;
    onsave?: () => void;
    oncursor?: (line: number) => void;
  } = $props();

  let host: HTMLDivElement;
  let view: EditorView | undefined;
  const ro = new Compartment();
  const lang = new Compartment();
  // A narrow pane (browser beside the terminal) soft-wraps long lines, such
  // as the flow-style port lines, instead of cutting them off.
  const wrap = new Compartment();
  const WRAP_BELOW_PX = 720;
  let wrapped = false;
  // The schema is read through a getter, so completion sees updates.
  let currentSchema: JS | null = null;
  $effect(() => {
    currentSchema = schema;
  });

  // Markdown support pulls HTML, CSS and JS modes: load it only for .md files.
  function langExt(l: string) {
    if (l === 'markdown') {
      void import('@codemirror/lang-markdown').then((m) => {
        if (language === 'markdown') view?.dispatch({ effects: lang.reconfigure(m.markdown()) });
      });
      return [];
    }
    if (l === 'yaml' || l === 'json') return yaml();
    return [];
  }

  // Colours from the theme tokens, so that both themes read well (the
  // default style is made for white and has dark-red strings): keys blue,
  // values green, as in the deck and the read-only YamlView.
  const highlight = HighlightStyle.define([
    { tag: [t.propertyName, t.definition(t.propertyName)], color: 'var(--info)' },
    { tag: [t.string, t.content, t.special(t.string), t.attributeValue], color: 'var(--ok)' },
    { tag: [t.number, t.bool, t.null, t.atom], color: 'var(--warn)' },
    { tag: [t.typeName, t.labelName, t.keyword], color: 'var(--warn)' },
    { tag: [t.comment, t.lineComment], color: 'var(--muted)', fontStyle: 'italic' },
    { tag: [t.separator, t.punctuation, t.meta, t.processingInstruction], color: 'var(--muted)' },
    // Markdown (knowledge articles)
    { tag: t.heading, color: 'var(--ink)', fontWeight: '700' },
    { tag: t.strong, fontWeight: '700' },
    { tag: t.emphasis, fontStyle: 'italic' },
    { tag: [t.link, t.url], color: 'var(--info)' },
    { tag: t.monospace, color: 'var(--ok)' },
    { tag: t.quote, color: 'var(--muted)' },
    { tag: t.invalid, color: 'var(--bad)' },
  ]);

  onMount(() => {
    let cursorTimer: ReturnType<typeof setTimeout> | undefined;
    view = new EditorView({
      parent: host,
      state: EditorState.create({
        doc: value,
        extensions: [
          basicSetup,
          syntaxHighlighting(highlight),
          lang.of(langExt(language)),
          wrap.of([]),
          ro.of([EditorState.readOnly.of(readOnly), EditorView.editable.of(!readOnly)]),
          lintGutter(),
          autocompletion({ override: [schemaCompletion(() => currentSchema)], icons: false }),
          schemaHover(() => currentSchema),
          protectSecrets(),
          keymap.of([
            {
              key: 'Mod-s',
              preventDefault: true,
              run: () => {
                onsave?.();
                return true;
              },
            },
          ]),
          EditorView.updateListener.of((u) => {
            if (u.docChanged) {
              const next = u.state.doc.toString();
              if (next !== value) value = next;
            }
            if (u.selectionSet && oncursor) {
              clearTimeout(cursorTimer);
              cursorTimer = setTimeout(() => oncursor?.(u.state.doc.lineAt(u.state.selection.main.head).number), 400);
            }
          }),
          EditorView.theme({
            '&': { height: '100%', fontSize: '13.5px', backgroundColor: 'var(--panel)', color: 'var(--ink)' },
            '.cm-scroller': { fontFamily: 'var(--mono)', lineHeight: '1.65' },
            '.cm-gutters': { backgroundColor: 'var(--panel)', color: 'var(--muted)', border: 'none' },
            '.cm-activeLine': { backgroundColor: 'color-mix(in srgb, var(--accent-soft) 45%, transparent)' },
            '.cm-activeLineGutter': { backgroundColor: 'transparent', color: 'var(--ink)' },
            '.cm-content': { caretColor: 'var(--ink)' },
            '&.cm-focused .cm-cursor': { borderLeftColor: 'var(--ink)' },
            '.cm-selectionBackground, &.cm-focused .cm-selectionBackground': { backgroundColor: 'color-mix(in srgb, var(--info) 22%, transparent) !important' },
            '.cm-tooltip': { border: '1px solid var(--line)', backgroundColor: 'var(--panel)', borderRadius: '6px', boxShadow: 'var(--shadow)' },
            '.cm-tooltip-autocomplete ul li[aria-selected]': { backgroundColor: 'var(--accent-soft)', color: 'var(--ink)' },
            '.cm-pk-hover': { padding: '8px 10px', maxWidth: '420px', fontFamily: 'var(--sans)', fontSize: '12.5px', lineHeight: '1.5' },
            '.cm-pk-hover code': { color: 'var(--muted)', fontSize: '11.5px' },
            '.cm-pk-secret': { backgroundColor: 'var(--line)', borderRadius: '3px', color: 'var(--muted)' },
            '.cm-diagnostic': { fontFamily: 'var(--sans)', fontSize: '12.5px' },
          }),
        ],
      }),
    });
    const ro2 = new ResizeObserver(() => {
      const narrow = host.clientWidth > 0 && host.clientWidth < WRAP_BELOW_PX;
      if (narrow === wrapped || !view) return;
      wrapped = narrow;
      view.dispatch({ effects: wrap.reconfigure(narrow ? EditorView.lineWrapping : []) });
    });
    ro2.observe(host);
    return () => {
      ro2.disconnect();
      clearTimeout(cursorTimer);
      view?.destroy();
      view = undefined;
    };
  });

  // Outside changes of value (form edits, reloads) replace the document.
  $effect(() => {
    const v = value;
    if (!view) return;
    const cur = view.state.doc.toString();
    if (v === cur) return;
    const sel = view.state.selection.main.head;
    view.dispatch({
      changes: { from: 0, to: cur.length, insert: v },
      selection: EditorSelection.cursor(Math.min(sel, v.length)),
      filter: false,
    });
  });

  $effect(() => {
    const r = readOnly;
    view?.dispatch({ effects: ro.reconfigure([EditorState.readOnly.of(r), EditorView.editable.of(!r)]) });
  });

  $effect(() => {
    const l = language;
    view?.dispatch({ effects: lang.reconfigure(langExt(l)) });
  });

  $effect(() => {
    const ps = problems;
    void value;
    if (!view) return;
    view.dispatch(setDiagnostics(view.state, toDiagnostics(view.state.doc, ps)));
  });

  $effect(() => {
    const ln = line;
    if (!view || !ln || ln < 1) return;
    const l = view.state.doc.line(Math.min(ln, view.state.doc.lines));
    view.dispatch({ selection: EditorSelection.cursor(l.from), effects: EditorView.scrollIntoView(l.from, { y: 'center' }) });
  });
</script>

<div class="cm-host" bind:this={host}></div>

<style>
  .cm-host {
    height: 100%;
    min-height: 0;
    overflow: hidden;
  }
  .cm-host :global(.cm-editor) {
    height: 100%;
  }
  .cm-host :global(.cm-editor.cm-focused) {
    outline: none;
  }
</style>
