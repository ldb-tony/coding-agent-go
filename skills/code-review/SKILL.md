---
name: code-review
description: Review code changes for correctness, edge cases, and consistency with project conventions. Use when the user asks for a code review, a diff review, or a quality check.
---

# Code Review

Review the requested files or diff with this checklist, and report findings grouped
by severity. Do not rewrite code unless the user asks for a fix.

## Checklist

1. **Correctness** — does the code do what it claims? Look for off-by-one errors,
   inverted conditions, unhandled error returns, and ignored return values.
2. **Edge cases** — empty input, single element, oversized input, concurrent access,
   and platform differences (path separators, line endings).
3. **Boundaries** — every external input (user input, files, API responses) must be
   validated where it enters the system, not deep inside.
4. **Conventions** — naming, file layout, and error-message style must match the
   surrounding code rather than the reviewer's personal taste.
5. **Tests** — new behavior should have a test; changed behavior should have an
   updated test.

## Report format

For each finding, give: file and line, severity (`high` / `medium` / `low`),
what is wrong, and a concrete suggestion. If you find nothing, say so explicitly —
do not invent findings.
