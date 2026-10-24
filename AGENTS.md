# Agent instructions

## Session-note workflow for substantial work

For a large change or other substantial task — for example, cross-subsystem
work, protocol or lifecycle changes, uncertain debugging, or a multi-step
implementation — use one or more session notes as the task's living record:

1. **Start a new note.** Create the next numbered file in
   `.ai/memory/sessions/` (for example, `40.md`), with the usual `title` and
   `date` frontmatter. Record the request, scope, and current status. Trivial or
   tightly scoped changes do not require a session note.
2. **Investigate before designing.** Read the relevant implementation and
   tests. Distinguish verified facts from assumptions and initial leads. Record
   useful findings, especially anything that could change the design.
3. **Write the plan before executing it.** Put the proposed behavior,
   implementation steps, test approach, and important constraints in the note or notes.
   Do not begin a substantial implementation with the plan existing only in
   chat.
4. **Execute the plan.** Keep the note or notes current as work proceeds. Document
   material intermediate findings, review feedback, failed reproductions, and
   any plan corrections — not just the final successful path. Update the plan
   when the implementation direction changes.
5. **Validate the result.** Run the focused tests and appropriately broader
   build, lint, type, race, or integration checks. Never claim a check passed
   unless it was actually run and observed passing.
6. **Write back the outcome.** Finish the note or notes with what changed, relevant
   file references, intermediate findings and corrections, exact validation
   commands and outcomes, and remaining risks or test blind spots. Mark whether
   the work is complete, still in progress, or blocked, and record useful next
   steps when applicable.

The note or notes are part of the deliverable, not an optional postscript. Keep
them factual and concise enough to remain useful to the next session.
