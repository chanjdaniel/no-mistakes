package agent

// MemoryFilesRule limits edits a pipeline agent initiates on its own. A memory
// file that is part of the change under validation is still reviewed like any
// other file, and a fix agent may address a finding or recorded human fix
// decision about that change. The document step owns a narrower
// correction-only rule (internal/pipeline/steps/document.go). This is a prompt
// contract, not a sandbox: agents keep free file access, so the behavioral
// tests in internal/pipeline/steps pin the wording on the emitted prompts.
const MemoryFilesRule = `

Agent memory files (AGENTS.md and CLAUDE.md) - limits on your own changes:
- This rule governs edits you would initiate on your own as pipeline work. Do not independently create, modify, rename, or delete these files, and do not add or rewrite their content just because something seems missing, stale, or wrong.
- If these files are part of the change under validation, review their changes like any other file. Never flag them merely for being changed; you may report inaccurate content as you would in any other file.
- In a fix turn, when a finding or recorded human fix decision concerns memory-file content that is part of the change under validation, you may edit those files to address it. Do not make unrelated or otherwise unprompted memory-file edits.
- They load into every future agent session, so additions or rewrites outside those prompted changes remain a deliberate human decision, not automated pipeline output.`
