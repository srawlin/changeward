# Compensation manifests

A compensation manifest tells Changeward's `httpapi` adapter how to undo REST calls: which request reads the
resource's state before a change, and which request reverses it. Calls without a matching entry are treated as
irreversible, so policy can require approval for them.

See `adapters/httpapi/httpapi.go` for the format. Contributions are welcome: one file per API, with the templates
`{param}`, `{response.field}`, `{body.field}`, `{before}` and `{before.field}`. Mark entries `compensatable` when
the undo is semantic (for example, a resource is re-created with a new id), and `irreversible` when no undo exists
(for example, sending an email or merging a pull request).
