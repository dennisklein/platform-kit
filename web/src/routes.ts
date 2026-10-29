// Route table. Each page gets { params, query } props. Phase 2 replaces
// the stub components; keep the patterns stable, agents and the focus
// store use them (Focus.route).
import type { RouteDef } from './lib/router.svelte';
import ChangesPage from './routes/changes/ChangesPage.svelte';
import ChangeDetail from './routes/changes/ChangeDetail.svelte';
import EditorPage from './routes/editor/EditorPage.svelte';
import ReviewPage from './routes/review/ReviewPage.svelte';
import LabRunPage from './routes/lab/LabRunPage.svelte';
import LabsPage from './routes/labs/LabsPage.svelte';
import AssetsPage from './routes/assets/AssetsPage.svelte';
import RackPage from './routes/assets/RackPage.svelte';
import EntityPage from './routes/assets/EntityPage.svelte';
import KbPage from './routes/kb/KbPage.svelte';
import ArticlePage from './routes/kb/ArticlePage.svelte';
import WorkPage from './routes/work/WorkPage.svelte';
import HistoryPage from './routes/history/HistoryPage.svelte';
import HistoryEntryPage from './routes/history/HistoryEntryPage.svelte';
import ChatPage from './routes/chat/ChatPage.svelte';
import ClustersPage from './routes/clusters/ClustersPage.svelte';
import LlmLogPage from './routes/llm-log/LlmLogPage.svelte';
import SettingsPage from './routes/settings/SettingsPage.svelte';
import AgentsPage from './routes/settings/AgentsPage.svelte';

export const routes: RouteDef[] = [
  { name: 'changes', pattern: '/changes', title: 'Changes', component: ChangesPage, nav: 'changes', rail: true, view: 'change' },
  { name: 'change', pattern: '/changes/:id', title: 'Change', component: ChangeDetail, nav: 'changes', rail: true, view: 'change' },
  // ?path=<file>&line=<n>
  { name: 'change.files', pattern: '/changes/:id/files', title: 'Files', component: EditorPage, nav: 'files', view: 'editor' },
  // ?tab=impact|semantic|source|rendered|overlap|risk
  { name: 'change.review', pattern: '/changes/:id/review', title: 'Review', component: ReviewPage, nav: 'changes', rail: true, view: 'review' },
  // latest run of the change; ?run=<run id> for another one
  { name: 'change.lab', pattern: '/changes/:id/lab', title: 'Lab', component: LabRunPage, nav: 'labs', rail: true, view: 'lab' },
  // main, read only (no change selected); ?path=
  { name: 'files', pattern: '/files', title: 'Files', component: EditorPage, nav: 'files', view: 'editor' },
  { name: 'labs', pattern: '/labs', title: 'Labs', component: LabsPage, nav: 'labs', view: 'lab' },
  // run id is "<change>/run-<n>", encoded as one segment
  { name: 'lab.run', pattern: '/labs/runs/:run', title: 'Lab run', component: LabRunPage, nav: 'labs', view: 'lab' },
  { name: 'assets', pattern: '/assets', title: 'Assets', component: AssetsPage, nav: 'assets', view: 'entity' },
  { name: 'rack', pattern: '/assets/racks/:name', title: 'Rack', component: RackPage, nav: 'assets', view: 'entity' },
  { name: 'entity', pattern: '/assets/:kind/:name', title: 'Entity', component: EntityPage, nav: 'assets', view: 'entity' },
  { name: 'kb', pattern: '/kb', title: 'Knowledge', component: KbPage, nav: 'kb', view: 'kb' },
  // ?path=kb/runbooks/add-node.md
  { name: 'kb.article', pattern: '/kb/article', title: 'Article', component: ArticlePage, nav: 'kb', view: 'kb' },
  { name: 'work', pattern: '/work', title: 'Work', component: WorkPage, nav: 'work', view: 'work' },
  // ?entity=cn-003&path=...&at=<sha> (time travel)
  { name: 'history', pattern: '/history', title: 'History', component: HistoryPage, nav: 'history', view: 'history' },
  { name: 'history.entry', pattern: '/history/:sha', title: 'History', component: HistoryEntryPage, nav: 'history', view: 'history' },
  { name: 'chat', pattern: '/chat', title: 'Chat', component: ChatPage, nav: 'chat', view: 'chat' },
  { name: 'clusters', pattern: '/clusters', title: 'Clusters', component: ClustersPage, nav: 'clusters' },
  { name: 'llm-log', pattern: '/llm-log', title: 'LLM log', component: LlmLogPage, nav: 'llm-log' },
  { name: 'settings', pattern: '/settings', title: 'Settings', component: SettingsPage, nav: 'settings' },
  { name: 'agents', pattern: '/settings/agents', title: 'Agents', component: AgentsPage, nav: 'settings' },
];
