<script lang="ts">
  // Files of a change (or main, read only): tree, tabs, YAML editor with
  // schema, form view, problems/kind/secrets panel, autosave to the worktree
  // and the commit panel (?commit=1).
  import { onMount, tick, untrack } from 'svelte';
  import type { Component } from 'svelte';
  import type { Params } from '../../lib/router.svelte';
  import { router, href } from '../../lib/router.svelte';
  import { api, must, errorText, ApiError } from '../../lib/api/client';
  import type {
    FileContent,
    FileNode,
    FileTree as Tree,
    Kind,
    Problem,
    Suggestion,
    WorktreeStatus,
  } from '../../lib/api/types';
  import { at, fields, type JS } from '../../lib/change/schema';
  import { readData, setIn, appendIn, deleteIn, type KeyOrder, type Path } from '../../lib/change/yamlEdit';
  import { baseName, shortPath, shortSha, timeAgo } from '../../lib/change/format';
  import { change as changeStore } from '../../lib/stores/change.svelte';
  import { changes } from '../../lib/stores/changes.svelte';
  import { events } from '../../lib/stores/events.svelte';
  import { toasts } from '../../lib/stores/toasts.svelte';
  import { entities } from '../../lib/change/entities.svelte';
  import { presenceFor } from '../../lib/change/presence.svelte';
  import Icon from '../../lib/components/Icon.svelte';
  import StateChip from '../../lib/components/StateChip.svelte';
  import AgentBanner from '../../lib/components/AgentBanner.svelte';
  import FileTree from './FileTree.svelte';
  import SchemaForm from './SchemaForm.svelte';
  import EditorSide from './EditorSide.svelte';
  import CommitPanel from './CommitPanel.svelte';
  import NewFileDialog from './NewFileDialog.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  const id = $derived(params.id as string | undefined);
  const ref = $derived(id ?? 'main');
  const path = $derived(query.get('path') ?? '');
  const qline = $derived(Number(query.get('line')) || 0);
  const commitMode = $derived(!!id && query.get('commit') === '1');
  const ch = $derived(id && changeStore.data?.id === id ? changeStore.data : null);

  // The YAML editor (CodeMirror) is loaded on demand, so other pages stay small.
  let YamlEditor = $state<Component<any> | null>(null);
  onMount(() => {
    void import('./YamlEditor.svelte').then((m) => (YamlEditor = m.default));
  });

  // --- tree, status, kinds ---------------------------------------------
  let tree = $state<Tree | null>(null);
  let status = $state<WorktreeStatus | null>(null);
  let kinds = $state<Kind[]>([]);
  let treeError = $state<string | null>(null);
  let openDirs = $state(new Set<string>());
  let newFileOpen = $state(false);

  async function loadTree() {
    const r = ref;
    try {
      const t = id
        ? await must(api.GET('/changes/{id}/tree', { params: { path: { id } } }))
        : await must(api.GET('/refs/{ref}/tree', { params: { path: { ref: 'main' } } }));
      if (r !== ref) return;
      tree = t;
      treeError = null;
      if (openDirs.size === 0) openDirs = initialOpen(t.entries, path);
    } catch (e) {
      treeError = errorText(e);
    }
  }

  async function loadStatus() {
    if (!id) {
      status = null;
      return;
    }
    try {
      status = await must(api.GET('/changes/{id}/status', { params: { path: { id } } }));
    } catch {
      status = null;
    }
  }

  function initialOpen(nodes: FileNode[], p: string): Set<string> {
    const s = new Set<string>();
    const walk = (ns: FileNode[]) => {
      for (const n of ns) if (n.type === 'dir' && (n.changed || (p && p.startsWith(n.path + '/')))) {
        s.add(n.path);
        walk(n.children ?? []);
      }
    };
    walk(nodes);
    return s;
  }

  function toggle(p: string) {
    const s = new Set(openDirs);
    if (s.has(p)) s.delete(p);
    else s.add(p);
    openDirs = s;
  }

  const dirty = $derived(new Set((status?.files ?? []).filter((f) => !f.committed).map((f) => f.path)));

  // --- open file ---------------------------------------------------------
  let file = $state<FileContent | null>(null);
  let fileError = $state<string | null>(null);
  let text = $state('');
  let saved = $state('');
  // The file as loaded: form edits keep the quoting style it has there.
  let origin = '';
  let saving = $state(false);
  let saveError = $state<string | null>(null);
  let stale = $state(false);
  let savedAt = $state<string | null>(null);
  let impactText = $state<string | null>(null);
  let suggestions = $state<Suggestion[]>([]);
  let jumpLine = $state(0);
  let tabs = $state<string[]>([]);
  const schemas = $state<Record<string, JS | null>>({});
  const readOnly = $derived(!id || !!file?.readOnly || (ch ? !ch.local || ['merged', 'deployed', 'verified', 'closed'].includes(ch.state) : false));

  const kind = $derived(file?.kind ? kinds.find((k) => k.name === file!.kind!.name) : undefined);
  const schemaKey = $derived(file?.kind ? `${file.kind.name}@${file.kind.version ?? ''}` : '');
  const schema = $derived(schemaKey ? (schemas[schemaKey] ?? null) : null);
  const parsed = $derived(file?.language === 'yaml' ? readData(text) : null);
  const isDirty = $derived(!!file && text !== saved);

  // Server problems (from the last read/save) plus the local YAML syntax error.
  const problems = $derived.by(() => {
    const ps: Problem[] = [...(file?.problems ?? [])].filter((p) => !((isDirty || parsed?.error) && p.source === 'yaml'));
    if (parsed?.error && file)
      ps.unshift({ path: file.path, line: parsed.error.line, severity: 'error', message: parsed.error.message, source: 'yaml' });
    return ps;
  });

  async function loadFile(p: string) {
    if (!p) {
      file = null;
      return;
    }
    const r = ref;
    fileError = null;
    try {
      const f = id
        ? await must(api.GET('/changes/{id}/file', { params: { path: { id }, query: { path: p } } }))
        : await must(api.GET('/refs/{ref}/file', { params: { path: { ref: 'main' }, query: { path: p, convert: false } } }));
      if (r !== ref || p !== path) return;
      file = f;
      text = f.content;
      saved = f.content;
      origin = f.content;
      stale = false;
      saveError = null;
      if (f.kind) void loadSchema(f.kind.name, f.kind.version);
      void loadSuggestions();
      if (qline) jump(qline);
    } catch (e) {
      file = null;
      fileError = errorText(e);
    }
  }

  async function loadSchema(name: string, version?: string) {
    const key = `${name}@${version ?? ''}`;
    if (key in schemas) return;
    schemas[key] = null;
    try {
      schemas[key] = (await must(
        api.GET('/kinds/{kind}/schema', { params: { path: { kind: name }, query: { version } } }),
      )) as JS;
    } catch {
      schemas[key] = null;
    }
  }

  async function loadSuggestions() {
    if (!id || !path) {
      suggestions = [];
      return;
    }
    try {
      const all = await must(api.GET('/changes/{id}/suggestions', { params: { path: { id } } }));
      suggestions = all.filter((s) => s.path === path && s.status === 'pending');
    } catch {
      suggestions = [];
    }
  }

  async function jump(n: number) {
    jumpLine = 0;
    await tick();
    jumpLine = n;
  }

  // --- save ----------------------------------------------------------------
  let saveTimer: ReturnType<typeof setTimeout> | undefined;
  $effect(() => {
    const t = text;
    if (!file || readOnly || t === untrack(() => saved)) return;
    clearTimeout(saveTimer);
    // Broken YAML is not saved on its own (it would break the render); Ctrl+S saves it anyway.
    if (untrack(() => parsed?.error)) return;
    saveTimer = setTimeout(() => void save(), 750);
  });

  async function save(force = true) {
    clearTimeout(saveTimer);
    if (!force && parsed?.error) return;
    if (!file || !id || readOnly || saving || stale) return;
    const content = text;
    if (content === saved) return;
    saving = true;
    saveError = null;
    try {
      const r = await must(
        api.PUT('/changes/{id}/file', {
          params: { path: { id }, query: { path: file.path } },
          body: { content, baseSha: file.sha },
        }),
      );
      saved = content;
      // Keep what the user typed meanwhile; take the rest of the server view.
      file = { ...r.file, content, kind: r.file.kind ?? (parsed?.error ? file.kind : undefined) };
      if (text === content && r.file.content !== content && !r.file.content.includes('••••')) {
        text = r.file.content;
        saved = r.file.content;
      }
      savedAt = new Date().toISOString();
      if (r.impact) impactText = r.impact.text;
      entities.refresh(id);
      void loadStatus();
      void loadTree();
      changes.later(400);
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) stale = true;
      else saveError = errorText(e);
    } finally {
      saving = false;
      // More typing while the request ran.
      if (!stale && file && text !== saved) saveTimer = setTimeout(() => void save(false), 300);
    }
  }

  async function reloadStale() {
    stale = false;
    await loadFile(path);
  }

  // --- form edits (client side, keep comments and style) -----------------
  // New keys go where the schema (and the form) has them.
  const keyOrder: KeyOrder = (pp) => {
    if (!schema) return undefined;
    const sub = at(schema, pp);
    return sub ? fields(schema, sub).map((f) => f.key) : undefined;
  };
  function onset(p: Path, v: unknown) {
    text = setIn(text, p, v, keyOrder, origin);
  }
  function onappend(p: Path, v: unknown) {
    text = appendIn(text, p, v, keyOrder);
  }
  function ondelete(p: Path) {
    text = deleteIn(text, p);
  }

  // --- view mode per kind ---------------------------------------------------
  let mode = $state<'yaml' | 'form'>('yaml');
  const formAvailable = $derived(!!schema && file?.language === 'yaml');
  $effect(() => {
    const k = file?.kind?.name;
    if (!k) return;
    let m: string | null = null;
    try {
      m = localStorage.getItem(`pk.editor.mode.${k}`);
    } catch {
      /* no storage */
    }
    mode = m === 'form' ? 'form' : 'yaml';
  });
  function setMode(m: 'yaml' | 'form') {
    mode = m;
    try {
      if (file?.kind) localStorage.setItem(`pk.editor.mode.${file.kind.name}`, m);
    } catch {
      /* no storage */
    }
  }

  // --- tabs -------------------------------------------------------------------
  $effect(() => {
    const p = path;
    if (p && !untrack(() => tabs).includes(p)) tabs = [...untrack(() => tabs), p].slice(-6);
  });

  function openPath(p: string, extra: Record<string, string | undefined> = {}) {
    if (isDirty) void save();
    router.navigate(href(id ? `/changes/${id}/files` : '/files', { path: p, ...extra }).slice(1));
  }

  function closeTab(p: string, e: Event) {
    e.stopPropagation();
    const i = tabs.indexOf(p);
    tabs = tabs.filter((t) => t !== p);
    if (p === path) {
      const next = tabs[Math.min(i, tabs.length - 1)];
      openPath(next ?? '');
    }
  }

  // --- loading --------------------------------------------------------------
  $effect(() => {
    changeStore.use(id ?? null);
  });

  $effect(() => {
    void ref;
    untrack(() => {
      tree = null;
      openDirs = new Set();
      tabs = path ? [path] : [];
      void loadTree();
      void loadStatus();
    });
  });

  $effect(() => {
    const p = path;
    void ref;
    untrack(() => {
      clearTimeout(saveTimer);
      void loadFile(p);
    });
  });

  // Open the folders above the open file (new files, links from elsewhere).
  $effect(() => {
    const p = path;
    if (!p || !tree) return;
    untrack(() => {
      const parts = p.split('/');
      const need = parts.slice(0, -1).map((_, i) => parts.slice(0, i + 1).join('/'));
      if (need.every((d) => openDirs.has(d))) return;
      openDirs = new Set([...openDirs, ...need]);
    });
  });

  // Keep the active tab visible in the tab strip.
  $effect(() => {
    void path;
    void tabs;
    void tick().then(() => document.querySelector('.tablist .tab.active')?.scrollIntoView({ block: 'nearest', inline: 'nearest' }));
  });

  // Keep the open file visible in the tree.
  $effect(() => {
    void tree;
    void path;
    void tick().then(() => document.querySelector('.treebox .node.active')?.scrollIntoView({ block: 'nearest' }));
  });

  $effect(() => {
    const n = qline;
    if (n) untrack(() => jump(n));
  });

  onMount(() => {
    void must(api.GET('/kinds')).then((k) => (kinds = k)).catch(() => (kinds = []));
    const offs = [
      events.on<{ changeId?: string; path?: string }>('change.updated', (d) => {
        if (!id || d.changeId !== id) return;
        void loadTree();
        void loadStatus();
        // Saved elsewhere (an agent, another tab): reload when the user has no edits.
        if (!isDirty && !saving && file) void refreshFile();
      }),
      events.on<{ changeId?: string; impact?: { text: string } }>('render.updated', (d) => {
        if (id && d.changeId === id && d.impact?.text) impactText = d.impact.text;
      }),
    ];
    return () => {
      offs.forEach((off) => off?.());
      clearTimeout(saveTimer);
      if (file && text !== saved && id) void save();
    };
  });

  async function refreshFile() {
    if (!id || !file) return;
    const p = file.path;
    try {
      const f = await must(api.GET('/changes/{id}/file', { params: { path: { id }, query: { path: p } } }));
      if (p !== path || isDirty || saving) return;
      if (f.sha !== file.sha) {
        file = f;
        text = f.content;
        saved = f.content;
      }
    } catch {
      /* gone or unreachable: keep the view */
    }
  }

  // --- presence -------------------------------------------------------------
  const presence = $derived(id ? presenceFor(id, ch?.presence) : null);
  const agentHere = $derived(presence && presence.path === path ? presence : null);

  // --- status line ----------------------------------------------------------
  const renderText = $derived(impactText ?? ch?.impact?.text ?? null);
  const saveText = $derived(
    readOnly
      ? 'read only'
      : stale
        ? 'changed elsewhere'
        : saving
          ? 'saving…'
          : isDirty
            ? parsed?.error
              ? 'not saved: fix the YAML (Ctrl+S saves anyway)'
              : 'editing'
            : savedAt
              ? `saved to worktree ${timeAgo(savedAt)}`
              : file?.status
                ? 'saved to worktree'
                : 'no edits',
  );

  // --- commit mode --------------------------------------------------------
  let staged = $state<Set<string>>(new Set());
  const commitFiles = $derived((status?.files ?? []).filter((f) => !f.committed));
  $effect(() => {
    const fs = commitFiles;
    untrack(() => {
      const next = new Set([...staged].filter((p) => fs.some((f) => f.path === p)));
      for (const f of fs) if (!knownFiles.has(f.path)) next.add(f.path);
      knownFiles = new Set(fs.map((f) => f.path));
      staged = next;
    });
  });
  let knownFiles = new Set<string>();
  function toggleStaged(p: string) {
    const s = new Set(staged);
    if (s.has(p)) s.delete(p);
    else s.add(p);
    staged = s;
  }

  // Narrow editor (projector zoom, side panel open): the kind/problems
  // column is a drawer over the editor, opened from the tab bar.
  let sideOn = $state(false);
  let seenSuggestions = 0;
  $effect(() => {
    const n = suggestions.length;
    if (n > untrack(() => seenSuggestions)) sideOn = true;
    seenSuggestions = n;
  });
  const problemCount = $derived(problems.filter((p) => p.severity !== 'info').length);
  const worst = $derived(problems.some((p) => p.severity === 'error') ? 'error' : problemCount ? 'warning' : 'none');

  const TABFLAG: Record<string, string> = { added: '+', untracked: '+', modified: 'M', deleted: 'D', renamed: 'R' };
</script>

<div class="edhost">
<div class="editor" class:commit={commitMode} class:side-on={sideOn}>
  <!-- left column: files, or the worktree in commit mode -->
  <aside class="files">
    {#if commitMode && ch}
      <div class="colhead"><span class="eyebrow">Worktree</span><span class="mono strong">{ch.id}</span></div>
      <ul class="stage">
        {#each commitFiles as f (f.path)}
          <li>
            <label>
              <input type="checkbox" checked={staged.has(f.path)} onchange={() => toggleStaged(f.path)} />
              <span class="mono">{f.path}</span>
              <span class="flag f-{f.status}">{TABFLAG[f.status] ?? ''}</span>
            </label>
          </li>
        {:else}
          <li class="muted empty">Nothing to commit. Edit files first.</li>
        {/each}
      </ul>
      <div class="colhead"><span class="eyebrow">Commits</span><span class="muted small">{ch.commits.length}</span></div>
      <ul class="commits">
        {#each ch.commits as c (c.sha)}
          <li title={c.subject}><span class="mono sha">{c.short || shortSha(c.sha)}</span> <span class="mono">{c.subject}</span></li>
        {:else}
          <li class="muted empty">No commits yet.</li>
        {/each}
      </ul>
      <div class="foot">
        <a class="btn small" href={href(`/changes/${id}/files`, { path: path || undefined })}><Icon name="file" size={14} />Back to files</a>
      </div>
    {:else}
      <div class="colhead">
        <span class="eyebrow">Files</span>
        <span class="mono strong">{id ?? 'main'}</span>
      </div>
      <div class="treebox">
        {#if tree}
          <FileTree nodes={tree.entries} active={path} open={openDirs} {dirty} onopen={(p) => openPath(p)} ontoggle={toggle} />
        {:else if treeError}
          <div class="error-box">{treeError}</div>
        {:else}
          <p class="muted pad">Loading files…</p>
        {/if}
      </div>
      {#if id && !readOnly}
        <div class="foot">
          <button class="btn small" onclick={() => (newFileOpen = true)}><Icon name="plus" size={14} />New file</button>
          <a class="btn small" class:dark={dirty.size > 0} href={href(`/changes/${id}/files`, { path: path || undefined, commit: '1' })}>
            <Icon name="commit" size={14} />Commit{dirty.size ? ` (${dirty.size})` : ''}
          </a>
        </div>
      {:else if !id}
        <div class="foot muted small">main, read only. Open a change to edit.</div>
      {/if}
    {/if}
  </aside>

  {#if commitMode && ch}
    <main class="main">
      <CommitPanel change={ch} paths={[...staged]} oncommitted={() => { void loadStatus(); void loadTree(); }} />
    </main>
  {:else}
    <main class="main">
      {#if agentHere}<AgentBanner presence={agentHere} />{/if}
      <div class="tabs">
        <div class="tablist" role="tablist">
          {#each tabs as t (t)}
            <div class="tab" class:active={t === path} role="tab" aria-selected={t === path}>
              <button class="tabbtn" onclick={() => openPath(t)} title={t}>
                {#if t === path && isDirty}<span class="dot editing" title="editing"></span>
                {:else if dirty.has(t)}<span class="dot" title="saved, not committed"></span>{/if}
                <span class="mono">{baseName(t)}</span>
              </button>
              <button class="x" aria-label="Close {baseName(t)}" onclick={(e) => closeTab(t, e)}>×</button>
            </div>
          {/each}
        </div>
        <div class="tabtools">
          {#if file}
            <button
              class="sidetoggle {worst}"
              class:on={sideOn}
              aria-expanded={sideOn}
              aria-controls="editor-side"
              title="Kind registry, problems and suggestions"
              onclick={() => (sideOn = !sideOn)}
            >
              Problems <span class="n">{problemCount}</span>{#if suggestions.length}<span class="sugg" title="{suggestions.length} suggestions">+{suggestions.length}</span>{/if}
            </button>
          {/if}
          {#if file && formAvailable}
            <div class="seg" role="group" aria-label="View">
              <button class:on={mode === 'yaml'} onclick={() => setMode('yaml')}>YAML</button>
              <button class:on={mode === 'form'} onclick={() => setMode('form')}>Form</button>
            </div>
          {/if}
        </div>
      </div>

      {#if stale}
        <div class="callout warn bar">
          <Icon name="warning" size={16} />
          <span>The file changed in the worktree since you opened it (an agent or another tab). Your edits are not saved.</span>
          <button class="btn small" onclick={reloadStale}>Reload file</button>
        </div>
      {/if}
      {#if saveError}
        <div class="callout bad bar">
          <Icon name="x" size={16} /><span>Not saved: {saveError}</span>
          <button class="btn small" onclick={() => save()}>Retry</button>
        </div>
      {/if}
      {#if file?.convertedFrom}
        <div class="callout bar">
          <Icon name="info" size={16} />
          <span>Stored as <code>{file.convertedFrom}</code>. The editor shows <code>{file.kind?.apiVersion}</code>; saving writes the newest version.</span>
        </div>
      {/if}

      <div class="body">
        {#if !path}
          <div class="empty-file">
            <Icon name="file" size={28} />
            <p>Open a file from the tree{id ? ', or create one from a kind' : ''}.</p>
            {#if id && !readOnly}<button class="btn" onclick={() => (newFileOpen = true)}><Icon name="plus" size={15} />New file</button>{/if}
          </div>
        {:else if fileError}
          <div class="pad"><div class="error-box">{fileError}</div></div>
        {:else if !file}
          <p class="muted pad">Loading {baseName(path)}…</p>
        {:else if mode === 'form' && formAvailable && schema}
          <div class="formwrap">
            {#if parsed?.error}
              <div class="callout bad">Fix the YAML first (line {parsed.error.line}: {parsed.error.message}). The form needs valid YAML.</div>
            {:else}
              <SchemaForm
                {schema}
                data={parsed?.data}
                {problems}
                {readOnly}
                changeId={id}
                secrets={file.secrets}
                {onset}
                {onappend}
                {ondelete}
              />
            {/if}
          </div>
        {:else if YamlEditor}
          {#key file.path}
            <YamlEditor
              bind:value={text}
              language={file.language}
              {readOnly}
              {problems}
              {schema}
              line={jumpLine}
              onsave={save}
            />
          {/key}
        {:else}
          <pre class="plain mono">{text}</pre>
        {/if}
      </div>

      <footer class="statusline mono">
        <span class="wt" title={ch?.worktree}>{id ? `worktree ${shortPath(ch?.worktree) || id}` : 'main'}</span>
        <span class="save" class:warn-text={stale || saveError} class:accent={isDirty || saving} title={saveText}>{saveText}</span>
        {#if file?.kind}<span class="muted kindname">{file.kind.name}{file.kind.objectName ? `/${file.kind.objectName}` : ''}</span>{/if}
        <span class="grow"></span>
        {#if id}<span class="render">render: {renderText ?? 'no change'}</span>{/if}
      </footer>
    </main>

    <div class="sidecol" id="editor-side">
      <button class="btn ghost small drawer-close" onclick={() => (sideOn = false)} title="Close"><Icon name="close" size={14} /><span class="sr-only">Close</span></button>
      {#if file}
        <EditorSide
          {file}
          {kind}
          {problems}
          bind:suggestions
          onjump={(p) => {
            if (p.line) {
              if (mode === 'form') setMode('yaml');
              void jump(p.line);
            }
          }}
          onsuggestion={() => setTimeout(() => void refreshFile(), 200)}
        />
      {:else if ch}
        <aside class="side-empty">
          <section class="card">
            <h3 class="eyebrow">Change</h3>
            <p class="title">{ch.title}</p>
            <p><StateChip state={ch.state} labState={ch.labState} /></p>
            <p class="muted small">{ch.files} files · {ch.commits.length} commits{ch.impact ? ` · ${ch.impact.text}` : ''}</p>
            <a class="btn small" href={href(`/changes/${ch.id}`)}>Open change</a>
          </section>
        </aside>
      {/if}
    </div>
  {/if}
</div>
</div>

{#if id}
  <NewFileDialog bind:open={newFileOpen} changeId={id} {kinds} oncreated={(p) => { void loadTree(); void loadStatus(); openPath(p); }} />
{/if}

<style>
  /* The layout follows the width the page gets (browser zoom, side panel). */
  .edhost {
    container-type: inline-size;
    height: 100%;
  }
  .editor {
    position: relative;
    display: grid;
    grid-template-columns: 330px minmax(0, 1fr) 380px;
    height: 100%;
    min-height: 0;
    background: var(--panel);
  }
  .editor.commit {
    grid-template-columns: 450px minmax(0, 1fr);
  }
  /* Commit view: only the right pane scrolls. */
  .editor.commit > .main {
    overflow: auto;
  }
  .files {
    display: flex;
    flex-direction: column;
    min-height: 0;
    border-right: 1px solid var(--line);
    background: var(--bg);
  }
  .colhead {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 0 16px;
    height: 50px;
    flex: none;
    border-bottom: 1px solid var(--line);
  }
  .strong {
    font-weight: 700;
    font-size: 14px;
  }
  .treebox {
    flex: 1;
    min-height: 0;
    overflow: auto;
    padding: 6px 0;
  }
  .foot {
    display: flex;
    gap: 8px;
    padding: 10px 12px;
    border-top: 1px solid var(--line);
    flex: none;
  }
  .stage,
  .commits {
    list-style: none;
    margin: 0;
    padding: 8px 0;
    border-bottom: 1px solid var(--line);
  }
  .commits {
    flex: 1;
    overflow: auto;
    border-bottom: 0;
  }
  .stage label {
    display: flex;
    gap: 10px;
    align-items: center;
    padding: 6px 16px;
    font-size: 14px;
    cursor: pointer;
  }
  .stage input {
    accent-color: var(--gsi-navy);
    width: 16px;
    height: 16px;
  }
  .stage .mono {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .flag {
    font: 700 12px var(--mono);
  }
  .f-added,
  .f-untracked {
    color: var(--ok);
  }
  .f-modified {
    color: var(--accent);
  }
  .f-deleted {
    color: var(--bad);
  }
  .commits li {
    padding: 6px 16px;
    font-size: 13.5px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .sha {
    color: var(--accent);
  }
  .empty {
    padding: 8px 16px;
    font-size: 13.5px;
  }
  .main {
    position: relative;
    display: flex;
    flex-direction: column;
    min-width: 0;
    min-height: 0;
  }
  .tabs {
    display: flex;
    align-items: stretch;
    justify-content: space-between;
    height: 50px;
    flex: none;
    border-bottom: 1px solid var(--line);
    background: var(--bg);
  }
  .tablist {
    display: flex;
    min-width: 0;
    overflow-x: auto;
    overflow-y: hidden;
    scrollbar-width: thin;
  }
  .tablist::-webkit-scrollbar {
    height: 4px;
  }
  .tablist::-webkit-scrollbar-thumb {
    background: var(--line-strong);
    border-radius: 2px;
  }
  .tabtools {
    display: flex;
    align-items: center;
    gap: 8px;
    flex: none;
    margin-left: 8px;
  }
  .sidetoggle {
    display: none;
    align-items: center;
    gap: 6px;
    border: 1px solid var(--line-strong);
    border-radius: 6px;
    background: var(--panel);
    color: var(--ink);
    padding: 4px 10px;
    font-size: 13px;
    font-weight: 600;
  }
  .sidetoggle.on {
    background: var(--accent-soft);
    border-color: var(--accent-fill);
  }
  .sidetoggle .n {
    border-radius: 8px;
    padding: 0 6px;
    font-size: 11px;
    background: var(--line);
    color: var(--ink);
  }
  .sidetoggle.error .n {
    background: var(--bad);
    color: #fff;
  }
  .sidetoggle.warning .n {
    background: var(--warn);
    color: #fff;
  }
  .sidetoggle .sugg {
    color: var(--info);
    font-size: 12px;
  }
  .drawer-close {
    display: none;
  }
  /* Tabs shrink (name ellipsized) before the strip scrolls; the active
     tab keeps its full name. */
  .tab {
    display: flex;
    align-items: center;
    border-right: 1px solid var(--line);
    background: transparent;
    flex: 0 1 auto;
    min-width: 72px;
  }
  .tab.active {
    flex-shrink: 0;
  }
  .tabbtn .mono {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .tab.active {
    background: var(--panel);
    box-shadow: inset 0 3px 0 var(--accent-fill);
    margin-bottom: -1px;
  }
  .tabbtn {
    display: flex;
    gap: 8px;
    align-items: center;
    border: 0;
    background: none;
    color: var(--ink);
    padding: 0 4px 0 14px;
    height: 100%;
    font-size: 14px;
    white-space: nowrap;
    min-width: 0;
    flex: 1 1 auto;
  }
  .tab:not(.active) .tabbtn {
    color: var(--muted);
  }
  .x {
    border: 0;
    background: none;
    color: var(--muted);
    font-size: 16px;
    padding: 0 10px 0 4px;
    flex: none;
    height: 100%;
  }
  .x:hover {
    color: var(--ink);
  }
  .dot {
    flex: none;
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--accent-fill);
  }
  .dot.editing {
    background: var(--info);
  }
  .seg {
    display: flex;
    align-self: center;
    margin-right: 14px;
    border: 1px solid var(--line-strong);
    border-radius: 6px;
    overflow: hidden;
    flex: none;
  }
  .seg button {
    border: 0;
    background: var(--panel);
    color: var(--ink);
    padding: 5px 14px;
    font-size: 14px;
  }
  .seg button.on {
    background: var(--gsi-navy);
    color: #fff;
  }
  .bar {
    display: flex;
    gap: 10px;
    align-items: center;
    margin: 10px 16px 0;
  }
  .bar span {
    flex: 1;
  }
  .body {
    flex: 1;
    min-height: 0;
    overflow: hidden;
    display: flex;
    flex-direction: column;
  }
  .body > :global(*) {
    flex: 1;
    min-height: 0;
  }
  .formwrap {
    overflow: auto;
    padding: 4px 8px 16px;
  }
  .plain {
    margin: 0;
    padding: 12px 16px;
    overflow: auto;
    font-size: 13.5px;
  }
  .empty-file {
    display: grid;
    place-content: center;
    justify-items: center;
    gap: 10px;
    color: var(--muted);
  }
  .pad {
    padding: 16px;
  }
  .statusline {
    display: flex;
    gap: 22px;
    align-items: center;
    height: 36px;
    flex: none;
    padding: 0 16px;
    border-top: 1px solid var(--line);
    background: var(--bg);
    font-size: 12.5px;
    color: var(--ink);
    white-space: nowrap;
    overflow: hidden;
  }
  /* The render counter never shrinks; the worktree path goes first. */
  .statusline > span {
    flex: none;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .statusline .wt {
    min-width: 0;
    flex: 0 1000 auto;
  }
  .statusline .kindname {
    min-width: 3em;
    flex: 0 10 auto;
  }
  .statusline .save {
    min-width: 6em;
    flex: 0 1 auto;
  }
  .statusline .render {
    flex: none;
  }
  .statusline .grow {
    flex: 1;
  }
  .accent {
    color: var(--accent);
  }
  .sidecol {
    position: relative;
    border-left: 1px solid var(--line);
    background: var(--bg);
    overflow: auto;
    min-height: 0;
  }
  .side-empty {
    padding: 16px;
  }
  .side-empty .card {
    padding: 14px 18px;
  }
  .side-empty p {
    margin: 0 0 8px;
  }
  .title {
    font-weight: 700;
  }
  @container (max-width: 1250px) {
    .editor {
      grid-template-columns: 260px minmax(0, 1fr) 300px;
    }
    .editor.commit {
      grid-template-columns: 380px minmax(0, 1fr);
    }
  }
  /* Narrow: two columns, so the editor keeps 600 px or more; kind and
     problems open as a drawer from the tab bar. */
  @container (max-width: 1100px) {
    .editor {
      grid-template-columns: 230px minmax(0, 1fr);
    }
    .editor.commit {
      grid-template-columns: 300px minmax(0, 1fr);
    }
    .sidecol {
      display: none;
      position: absolute;
      top: 0;
      right: 0;
      bottom: 0;
      width: min(360px, 85%);
      z-index: 20;
      box-shadow: var(--shadow), -8px 0 24px rgb(0 0 0 / 0.12);
    }
    .editor.side-on .sidecol {
      display: block;
      padding-top: 30px;
    }
    .sidetoggle {
      display: inline-flex;
    }
    .drawer-close {
      display: inline-flex;
      position: absolute;
      top: 4px;
      right: 8px;
      z-index: 1;
    }
    .statusline .wt {
      display: none;
    }
  }
  @container (max-width: 760px) {
    .editor,
    .editor.commit {
      grid-template-columns: 190px minmax(0, 1fr);
    }
  }
</style>
