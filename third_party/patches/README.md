# Source-build patches for the pinned rustpush tree

`make build` applies these unified diffs to the upstream tree it clones at the
SHA in `../rustpush-upstream.sha`, after the one-line patches carried inline
in the Makefile. They exist for changes too large to express as a Makefile
substitution. Each directory is named after the tree it applies to:

| Directory | Apply root |
| --- | --- |
| `apple-private-apis/` | `third_party/rustpush-upstream/third_party/apple-private-apis` |

The Makefile applies every patch through `rp_apply`, which verifies the
desired end state rather than trusting that the patch ran: a patch that is
already fully present is skipped, a tree that carries only part of it stops
the build, and a patch that no longer applies to the pinned tree stops the
build too. A patch that upstream adopts is deleted here together with its
Makefile line, in the same commit.

These are conventional diffs, so they can be reviewed and tried by hand:

```sh
git -C third_party/rustpush-upstream/third_party/apple-private-apis \
  apply --unidiff-zero --whitespace=fix --check \
  "$PWD/third_party/patches/apple-private-apis/<name>.patch"
```
