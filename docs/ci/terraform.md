# Terraform and OpenTofu

Changeward works with Terraform and OpenTofu in two places.

## In CI: `changeward tf review`

Your pipeline makes the plan, as usual. Changeward classifies every resource change, applies policy and posts a
review:

```sh
terraform plan -out tf.plan -input=false
terraform show -json tf.plan > plan.json
changeward tf review --plan-json plan.json --env prod            # exit 0 approved, 2 review required, 3 denied
changeward tf review --plan-json plan.json --env prod --github   # also comment on the PR (GITHUB_TOKEN)
```

```
## ⚠️ Changeward: Terraform plan · compensatable
**Policy (prod):** 👀 review required by owner:codeowners: change is destructive, not proven reversible, or failed rehearsal
0 to add, 0 to change, 1 to replace, 0 to destroy
   -/+ local_file.app_config
```

Classification:

- **Destroying or replacing a stateful resource is irreversible.** Stateful resources are databases, buckets,
  volumes, keys, DNS zones and similar. Add your own with `--stateful-types aws_foo,google_bar`.
- **Other changes are compensatable:** re-applying the previous configuration undoes them.
- Policy sees `change.destructive`, the reversibility, and the counts of each action.

A rule to protect data stores:

```yaml
rules:
  - name: no-stateful-destroy-in-prod
    when: change.adapter == "terraform" && change.env == "prod" && change.reversibility == "irreversible"
    decide: require
    approvers: ["group:platform"]
    quorum: 2
```

**GitHub Actions:**

```yaml
- uses: hashicorp/setup-terraform@v3
- run: terraform init -input=false && terraform plan -out tf.plan -input=false && terraform show -json tf.plan > plan.json
- run: changeward tf review --plan-json plan.json --env prod --github
  env: { GITHUB_TOKEN: "${{ github.token }}" }
```

**GitLab CI:**

```yaml
tf-review:
  image: hashicorp/terraform:1.16
  script:
    - terraform init -input=false && terraform plan -out tf.plan -input=false && terraform show -json tf.plan > plan.json
    - changeward tf review --plan-json plan.json --env prod
  rules: [{ if: $CI_PIPELINE_SOURCE == "merge_request_event" }]
```

For a GitLab job image without `changeward`, download the binary first, as the
[GitLab component](gitlab.md) does.

## Direct lane: agents applying Terraform

```sh
changeward connect infra --adapter terraform --env staging --option dir=./infra   # options: binary=tofu, var_files=…
changeward exec --target infra "apply"
```

Changeward runs the plan and shows it to policy, then applies **exactly that plan file**. If the configuration
or the state changed in the meantime, the apply is refused as stale.

Undo re-applies the last-applied configuration snapshot, not the agent's edited files. The previous
configuration comes from the last apply made through Changeward, or from the files at git `HEAD`. Without
either, undo refuses: restore the state backup instead. A hook blocks the agent's
own `terraform apply` / `destroy` when a Terraform target is registered.
