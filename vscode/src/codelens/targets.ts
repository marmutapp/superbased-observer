// Pure helper: classify an instruction-file path into the
// `observer suggest --target` argument. Kept separate from
// instructionFiles.ts so unit tests don't pull in `vscode`.

export type InstructionTarget = 'claude' | 'agents' | 'cursor';

const FILE_TO_TARGET: Array<{ name: string; target: InstructionTarget }> = [
  { name: 'CLAUDE.md', target: 'claude' },
  { name: 'AGENTS.md', target: 'agents' },
  { name: '.cursorrules', target: 'cursor' },
];

export function instructionTargetFor(filePath: string | undefined): InstructionTarget | undefined {
  if (!filePath) return undefined;
  const cleaned = filePath.replace(/[\\/]+$/, '');
  const idx = Math.max(cleaned.lastIndexOf('/'), cleaned.lastIndexOf('\\'));
  const base = idx >= 0 ? cleaned.slice(idx + 1) : cleaned;
  for (const entry of FILE_TO_TARGET) {
    if (base === entry.name) return entry.target;
  }
  return undefined;
}
