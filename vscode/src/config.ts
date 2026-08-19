// Seeding logic for `ACY: Create .acy.json` — pure, vscode-free, node-testable.

export type AgentName = 'claude' | 'codex';

/** The acy.defaults.* settings, as read from VS Code configuration. */
export interface Defaults {
  agent?: string;
  model?: string;
  claudeBin?: string;
  codexBin?: string;
  countdown?: string;
  log?: string;
  maxLines?: number;
  planTools?: string[];
  childModel?: string;
  childEffort?: string;
  taskBudget?: number;
  runBudget?: number;
  useApiKey?: boolean;
}

/**
 * Builds the object to seed a new .acy.json with. Only values the user
 * actually set make it in: the file's absent-vs-set distinction is load-bearing
 * (the binary treats a missing key as "keep the default"), so writing every
 * setting's zero value would pin defaults the user never chose. An empty
 * string can't be distinguished from "unset" in VS Code settings, so disabling
 * the log ("log": "") stays a hand edit — the schema documents it.
 */
export function buildConfigSeed(d: Defaults): Record<string, unknown> {
  const seed: Record<string, unknown> = {};
  if (d.agent === 'claude' || d.agent === 'codex') {
    seed.agent = d.agent;
  }
  if (d.model?.trim()) {
    seed.model = d.model.trim();
  }
  if (d.claudeBin?.trim()) {
    seed.claudeBin = d.claudeBin.trim();
  }
  if (d.codexBin?.trim()) {
    seed.codexBin = d.codexBin.trim();
  }
  if (d.countdown?.trim()) {
    seed.countdown = d.countdown.trim();
  }
  if (d.log?.trim()) {
    seed.log = d.log.trim();
  }
  if (typeof d.maxLines === 'number' && d.maxLines > 0) {
    seed.maxLines = d.maxLines;
  }
  if (d.planTools && d.planTools.length > 0) {
    seed.planTools = d.planTools;
  }
  if (d.useApiKey === true) {
    seed.useApiKey = true;
  }
  if (d.childModel?.trim()) {
    seed.childModel = d.childModel.trim();
  }
  if (d.childEffort?.trim()) {
    seed.childEffort = d.childEffort.trim();
  }
  if (typeof d.taskBudget === 'number' && d.taskBudget >= 0) {
    seed.taskBudget = d.taskBudget;
  }
  if (typeof d.runBudget === 'number' && d.runBudget >= 0) {
    seed.runBudget = d.runBudget;
  }
  return seed;
}

/** Renders the seed the way a human would have typed it. */
export function renderConfigSeed(seed: Record<string, unknown>): string {
  return JSON.stringify(seed, null, 2) + '\n';
}

/** Mirrors the CLI default: an existing project config without agent is Claude. */
export function selectAgent(
  projectAgent: unknown,
  projectConfigExists: boolean,
  defaultAgent: unknown,
): AgentName {
  if (projectAgent === 'claude' || projectAgent === 'codex') {
    return projectAgent;
  }
  if (projectConfigExists) {
    return 'claude';
  }
  return defaultAgent === 'codex' ? 'codex' : 'claude';
}
