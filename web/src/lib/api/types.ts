// Short names for the generated API schemas.
import type { components } from './schema';

type S = components['schemas'];

export type Me = S['Me'];
export type BenchStatus = S['BenchStatus'];
export type Focus = S['Focus'];
export type FocusUpdate = S['FocusUpdate'];
export type FocusView = S['FocusView'];
export type Presence = S['Presence'];
export type EventType = S['EventType'];
export type ChangeState = S['ChangeState'];
export type ChangeSummary = S['ChangeSummary'];
export type ChangeList = S['ChangeList'];
// Change is allOf(ChangeSummary, details) in the spec, and both parts have
// "commits" (a count in the summary, the commit list in the details). The
// API sends the list, so the detail type replaces the count.
export type Change = Omit<S['ChangeSummary'], 'commits'> &
  Omit<S['Change'], keyof S['ChangeSummary']> & { commits: S['Commit'][] };
export type ChangeGroup = S['ChangeGroup'];
export type LabState = S['LabState'];
export type IdentityKind = S['IdentityKind'];
export type RiskLevel = S['RiskLevel'];
export type ApiErrorBody = S['Error'];

// Change workflow (area E).
export type ApprovalCount = S['ApprovalCount'];
export type MergeRequestRef = S['MergeRequestRef'];
export type LifecycleStep = S['LifecycleStep'];
export type LabRunRef = S['LabRunRef'];
export type ChangeMeta = S['ChangeMeta'];
export type Approval = S['Approval'];
export type Overlap = S['Overlap'];
export type Commit = S['Commit'];
export type Trailer = S['Trailer'];
export type CreateChangeRequest = S['CreateChangeRequest'];
export type PushResult = S['PushResult'];
export type RebaseResult = S['RebaseResult'];
export type MergeResult = S['MergeResult'];
export type CommitMessageSuggestion = S['CommitMessageSuggestion'];
export type ChangeEvent = S['ChangeEvent'];
export type RenderEvent = S['RenderEvent'];
export type ForgeEvent = S['ForgeEvent'];
export type LabStepEvent = S['LabStepEvent'];
export type ClusterRollout = S['ClusterRollout'];
export type FileChange = S['FileChange'];
export type FileStatus = S['FileStatus'];
export type WorktreeStatus = S['WorktreeStatus'];
export type FileNode = S['FileNode'];
export type FileTree = S['FileTree'];
export type FileContent = S['FileContent'];
export type KindRef = S['KindRef'];
export type SecretField = S['SecretField'];
export type WriteFileResult = S['WriteFileResult'];
export type ImpactSummary = S['ImpactSummary'];
export type Problem = S['Problem'];
export type ProblemList = S['ProblemList'];
export type Impact = S['Impact'];
export type ClusterImpact = S['ClusterImpact'];
export type KustomizationImpact = S['KustomizationImpact'];
export type ObjectChange = S['ObjectChange'];
export type SemanticLine = S['SemanticLine'];
export type OpenStep = S['OpenStep'];
export type Risk = S['Risk'];
export type RiskReason = S['RiskReason'];
export type Diff = S['Diff'];
export type DiffViewKind = S['DiffViewKind'];
export type FileDiff = S['FileDiff'];
export type Gates = S['Gates'];
export type MatchedRule = S['MatchedRule'];
export type LabGateStatus = S['LabGateStatus'];
export type LabRun = S['LabRun'];
export type LabStep = S['LabStep'];
export type LabCheck = S['LabCheck'];
export type LabList = S['LabList'];
export type LabResult = S['LabResult'];
export type Kind = S['Kind'];
export type JsonSchema = S['JsonSchema'];
export type TruthClass = S['TruthClass'];
export type ApplyPath = S['ApplyPath'];
export type EntitySummary = S['EntitySummary'];
export type Rack = S['Rack'];
export type WorkItem = S['WorkItem'];
export type WorkBoard = S['WorkBoard'];
export type AdvisorRecipe = S['AdvisorRecipe'];
export type AdvisorResult = S['AdvisorResult'];
export type AdvisorPoint = S['AdvisorPoint'];
export type Source = S['Source'];
export type Suggestion = S['Suggestion'];
export type AgentGrant = S['AgentGrant'];
export type Cluster = S['Cluster'];
