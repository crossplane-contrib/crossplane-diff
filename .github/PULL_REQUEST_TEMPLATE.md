<!--
Thank you for helping to improve Crossplane! Please read the contribution docs
(linked below) if this is your first Crossplane pull request.
-->

### Description of your changes

<!--
Briefly describe what this pull request does, and how it is covered by tests.
Be proactive - direct your reviewers' attention to anything that needs special
consideration.

We love pull requests that fix an open issue. If yours does, use the below line
to indicate which issue it fixes, for example "Fixes #500".
-->

Fixes #

I have: <!--You MUST either [x] check or [ ] ~strike through~ every item.-->

- [ ] Read and followed Crossplane's [contribution process].
- [ ] Run `earthly -P +reviewable` to ensure this PR is ready for review.
- [ ] Added or updated unit tests.
- [ ] Added or updated integration or e2e tests.
<!--
Prefer integration tests (cmd/diff/*_integration_test.go,
envtest + real functions, seconds/case) for crossplane-diff's
own logic. Reserve e2e tests (test/e2e, minutes) for behavior
only a live Crossplane controller reconcile can prove.
-->
- [ ] Updated documentation as needed (user-facing behavior in `README.md`; architecture in `design/design-doc-cli-diff.md` and its diagrams).
<!--
Update the README for anything a user would notice (flags,
subcommands, exit codes, structured-output schema, rendered
output). Update the design doc + mermaid diagrams for
interface, client, renderer, workflow, or layout changes; see
"Keeping Documentation in Sync" in CLAUDE.md for the trigger
table. Strike through (~~...~~) if this change is not
user-facing and touches no documented behavior.
-->

Need help with this checklist? See the [cheat sheet].

[contribution process]: https://github.com/crossplane/crossplane/tree/main/contributing
[cheat sheet]: https://github.com/crossplane/crossplane/tree/main/contributing#checklist-cheat-sheet
