# Changeward GitLab CI/CD component

```yaml
include:
  - component: gitlab.com/rawlinson-software-inc-group/test/rehearse@main
    inputs:
      mode: report
      synthetic-rows: 1000
```

Set a masked CI/CD variable `CHANGEWARD_GITLAB_TOKEN` (a project access token with `api` scope) to post the report
as a merge request note and set a commit status. Without it the report is printed in the job log and kept as an
artifact. External status checks and deployment approvals (GitLab Ultimate) can consume the gate's decision in
server mode.
