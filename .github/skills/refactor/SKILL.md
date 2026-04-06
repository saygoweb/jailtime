---
name: refactor
description: Refactor code to resolve a described error, ensuring the fix is well-understood in the context of the codebase. Use when asked to refactor code or fix a bug or error.
---

Refactor code to resolve a described error, ensuring the fix is well-understood in the context of the codebase. Follow this workflow:

1. Ensure an error description is provided; elicit from user if missing.
2. Use `.github/copilot-instructions.md` and Serena project knowledge to understand the codebase and error context.
3. Propose a fix (multiple options if non-trivial, one if simple). Require user approval unless advance permission is given.
4. Create test(s) that expose the issue. Run tests; they should fail.
5. Refactor code to resolve the error. Re-run tests; they should now pass.
6. Commit the code with a clear message.
7. Only push and create a draft PR if the user explicitly requests it.

Always confirm the fix plan with the user unless advance permission is given. Use project documentation and codebase context to ensure high-quality, relevant fixes.
