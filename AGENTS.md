# Agent instructions

## Session-note workflow for substantial work

Every task must be completed end to end: start with a session note and finish
by writing the outcome back to that note. Stopping a review loop or any other
subtask does not stop the task. Use one or more session notes as the task's
living record:

1. **Start a new note.** Create the next numbered file in
   `.ai/memory/sessions/` (for example, `40.md`), with the usual `title` and
   `date` frontmatter. Record the request, scope, and current status.
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

## Code-review rules

1. Reviews must not be overly theoretical. Every reported issue must be
   reproducible or confirmed directly from the code, with concrete evidence.
2. The reviewer should make the first review round as comprehensive as possible
   to minimize review rounds and avoid repeated, endless reworks.
3. Minimize the number of review rounds; never maximize them. Seven rounds is a
   hard maximum, not a target or an expected number of rounds.
4. Use at most seven review rounds. If issues remain after the seventh round,
   the reviewer should fix them directly instead of requesting another rework.
