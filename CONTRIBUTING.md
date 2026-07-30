# Contributing

Short version: **pull requests are closed for now, and issues have no response
guarantee.** Please read on before you spend time on either.

## Pull requests are being closed

This is a hobby project maintained in spare time. Reviewing a patch properly
means reading it, understanding how it interacts with the FFI boundary and the
bridge's state handling, testing it against a live Apple account, and then
owning it forever. There isn't capacity for that right now, so incoming pull
requests are **closed automatically** rather than left to sit open for months.

That isn't a judgement on your patch. An unreviewed PR that stays open is worse
for you than a closed one — it looks like it might land, and it never does.

Closing doesn't delete anything: the diff stays on the PR and it can be reopened
if it turns out to be something the project wants. Please don't plan around that
happening.

## Issues are open, with no expectation attached

The issue tracker is open and you're welcome to file. Treat it as a place to
leave information for other users and for future-me, not as a support channel:

- **No expectation that an issue will be looked at**, answered, or acted on.
- No expectation of a timeline if one is.
- Bugs may be closed unread if the project moves past them.

If that's fine with you, a good report is still genuinely useful — especially
one that says which platform and release binary you're on, what you expected,
and what happened instead.

## Fork it

The project is [MPL-2.0](LICENSE), so the more satisfying option is available:
fork it and go. You don't need permission and you don't need to wait on a
review. `AGENTS.md` has the developer notes — the FFI regeneration steps, the
config source-of-truth rules, and the parts that bite.

If your fork ends up somewhere interesting, an issue saying so is welcome under
the terms above.
