# Instructions for coding agents

The Terraform and OpenTofu provider for Limen: Go, `terraform-plugin-framework`, a thin client of `limen.manage.v1`. Read [ADR-0042](https://github.com/limen-dev/limen/blob/main/docs/adr/0042-management-api.md) before changing behavior; the server's rules there decide what the provider may assume.

## Rules
- **Work in a git worktree:** `git worktree add .claude/worktrees/<topic> -b <branch>`. Never edit the main checkout or switch its branch.
- **The contract is Limen's.** `proto/limen/manage/v1` is a verbatim copy; change it only with `scripts/sync-proto.sh`. Never add fields here first.
- **Verify before claiming.** Run it before saying it works. Behavior checked only against `internal/fakelimen` is labelled *unverified against Limen*.
- **Model server behavior in fakelimen first**, with a test, before the provider relies on it.
- **Secrets stay out of logs and state:** tokens and keys are never logged, and sensitive attributes stay `Sensitive`.
- **Commits:** conventional, small, focused. Stage specific files, never `git add -A`. Don't push or tag unless asked. Pull requests as drafts.

## Commands
```sh
mise install
go build ./... && golangci-lint run ./... && go test ./...
make testacc && make testacc-tofu            # real CLIs against fakelimen
make generate && git diff --exit-code        # generated code and docs are current
scripts/sync-proto.sh --check ../limen origin/main
```
