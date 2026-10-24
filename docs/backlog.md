# Backlog

Agreed work that is not scheduled yet.

## Name and distribution

- **changeward.com → GitHub.** Forward the domain to https://github.com/srawlin/changeward, and set it as the
  repository's website. The repository stays the main site.
- **Reserve package names before the first release.** On 2026-10-22 all of these were free:
  - npm: create the `changeward` organization (needed for `@changeward/cli` and `@changeward/sdk`), and the
    unscoped `changeward` package.
  - PyPI: `changeward`, `changeward-sdk` and `changeward-contextforge` (a trusted-publisher entry reserves each one).
  - Homebrew tap `srawlin/homebrew-tap`, and `ghcr.io/srawlin/changeward`, both created by the release workflow.
- **GitHub organization.** A `ChangeWard` organization (created 2026-07-15, no public repositories) belongs to
  someone else. Packages and docs use `srawlin/changeward`, so nothing depends on it.
- **Make the repository public.** `go install github.com/srawlin/changeward/cmd/changeward@latest` and the
  `uses: srawlin/changeward/integrations/github-action@main` reference only work once it is.

## Deferred

- Slack approvals verified against a real workspace.
- Publishing to package registries (npm, PyPI, Homebrew, GHCR) from the release workflow.
- Adapters: MongoDB, then Redis/Valkey (proposed; not yet confirmed).
- A dedicated DevOps agent built on pi, as another distribution of the control plane: evaluated, with a gated
  prototype plan, in the "Should Changeward ship its own DevOps agent?" doc.
