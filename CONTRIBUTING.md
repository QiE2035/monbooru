# Contributing

## How to propose a change

1. Open an issue describing what you want to change and why. For
   anything beyond a small fix, do this before writing code (it
   avoids wasted work if the change does not fit the project).
2. If you have code, push it to a branch on your fork and link the
   branch in the issue.
3. The diff gets reviewed, and the outcome lands in the issue:
   accepted as-is, accepted in part, needs changes, or declined,
   with reasons.
4. Accepted work is applied upstream and appears here on the next
   release. Branches are never merged through the GitHub UI.

## Credit

Contributors are credited in the changelog entry of the release that
ships their work: a thanks line linking the issue, plus a
co-authored mention on the release commit. 

## What review expects

- Match the existing code style and search for
  a similar case before inventing a new pattern.
- Stdlib first. A new dependency needs a strong justification and
  should be discussed in the issue before any code is written.
- Small, focused changes review fast. Cross-cutting changes that
  touch many templates or handlers need discussion first.

Make sure you have reviewed every line of the diff yourself, and that you have carefully tested your code.

## License

Contributions are accepted under the repository LICENSE.
