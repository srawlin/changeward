# GitLab CI/CD

The `rehearse` CI/CD component does what the GitHub Action does. On every merge request pipeline, it rehearses
new migrations on a throwaway Postgres service. It posts or updates an MR note with the effect report, sets a
`changeward/rehearsal` commit status, and applies your policy.

**Verified on gitlab.com** (`rawlinson-software-inc-group/test`):

- **MR !1** drops a column. It was flagged irreversible, the note was posted, and the status was
  `pending: review required: owner:codeowners (0 of 1 approval(s) from someone other than the author)`.
- **MR !2** is reversible, in gate mode. It was auto-approved and the status was `success`.

## Use the component

`.gitlab-ci.yml`:

```yaml
include:
  - component: $CI_SERVER_FQDN/<group>/<changeward-project>/rehearse@<version>
    inputs:
      mode: report              # report | gate | deny-only
      synthetic-rows: "1000"    # T1; "0" = schema only
      environment: prod
      # migrations-dir: db/migrations      # default: detected
      # framework: flyway                  # default: detected
```

The job runs only in merge request pipelines (`$CI_PIPELINE_SOURCE == "merge_request_event"`). It uses
`alpine:3.20` with a `postgres:<postgres-version>-alpine` service.

**Notes and statuses:** add a masked CI/CD variable `CHANGEWARD_GITLAB_TOKEN`, holding a project access token
with `api` scope (`GITLAB_TOKEN` also works). Without it, the report is printed in the job log and kept as the
artifact `changeward-report.md`, along with `changeward-evidence.json`.

## Inputs

| Input | Default | Meaning |
| --- | --- | --- |
| `mode` | `report` | `report`: the job never fails. `gate`: fails unless approved, or review-required and satisfied by MR approvals. `deny-only`: fails only when denied. |
| `synthetic-rows` | `1000` | Synthetic rows per table |
| `environment` | `prod` | Environment class for policy |
| `postgres-version` | `18` | Postgres service image |
| `framework`, `migrations-dir` | detected | Overrides |
| `stage` | `test` | Pipeline stage |
| `version` | `latest` | Changeward release to download from GitHub, or `preinstalled` when your job image already has `changeward` |
| `binary-url` | — | Download the Linux tarball from here instead, e.g. your project's generic package registry. `CI_JOB_TOKEN` is sent for URLs on the same GitLab instance. |

Before the first public release, publish a build to your project's package registry and point `binary-url` at
it, as our test project does:

```sh
curl -H "PRIVATE-TOKEN: $TOKEN" --upload-file changeward_linux_amd64.tar.gz \
  "https://gitlab.com/api/v4/projects/<id>/packages/generic/changeward/0.1.0/changeward_linux_amd64.tar.gz"
```

The job runs on GitLab's shared Linux amd64 runners. The binary is linked against musl for Alpine; build it with
the repository's `Dockerfile` (`--target build`).

## Making it a merge gate

1. Use `mode: gate`, or keep `report` and rely on the commit status.
2. Turn on **Settings → Merge requests → Pipelines must succeed**.

   The commit status is part of the pipeline, so a `pending` `changeward/rehearsal` keeps the pipeline from
   succeeding. On MR !1 the pipeline stayed "running" until review.
3. Configure **approval rules** (or CODEOWNERS with code-owner approval) for the people your policy names.

When the MR's approvals satisfy the decision, the next pipeline passes: someone other than the author, every
named `user:` approver, and the policy's quorum. Approving doesn't start a pipeline by itself in GitLab. Click
**Run pipeline** on the MR, or push.

## Self-managed GitLab

Mirror the component into a project on your instance, and include it with
`component: $CI_SERVER_FQDN/<path>/rehearse@<ref>`. `CI_API_V4_URL` is picked up automatically for notes,
statuses and approvals.
