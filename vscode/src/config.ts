// Seeding logic for `ACY: Create .acy.json` — pure, vscode-free, node-testable.

/** The acy.defaults.* settings, as read from VS Code configuration. */
export interface Defaults {
  model?: string;
  claudeBin?: string;
  countdown?: string;
  log?: string;
  maxLines?: number;
  planTools?: string[];
  childModel?: string;
  childEffort?: string;
  taskBudget?: number;
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
  if (d.model?.trim()) {
    seed.model = d.model.trim();
  }
  if (d.claudeBin?.trim()) {
    seed.claudeBin = d.claudeBin.trim();
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
  return seed;
}

/** Renders the seed the way a human would have typed it. */
export function renderConfigSeed(seed: Record<string, unknown>): string {
  return JSON.stringify(seed, null, 2) + '\n';
}
