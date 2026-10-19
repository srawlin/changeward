# Changegate GitLab CI/CD component

```yaml
include:
  - component: gitlab.com/changegate/changegate/rehearse@0.1.0
    inputs:
      mode: report
      synthetic-rows: 1000
```

Set a masked CI/CD variable `CHANGEGATE_GITLAB_TOKEN` (a project access token with `api` scope) to post the report
as a merge request note and set a commit status. Without it the report is printed in the job log and kept as an
artifact. External status checks and deployment approvals (GitLab Ultimate) can consume the gate's decision in
server mode.
