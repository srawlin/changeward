---
description: Turn this session's schema changes into a migration and a pull request
argument-hint: [framework]
---
Explain that I need to run `changegate promote --pr` myself (promotion writes migration files from the recorded
changes and opens a PR with the session timeline as evidence). If I name a framework ($ARGUMENTS), include
`--framework $ARGUMENTS` in the command you suggest.
