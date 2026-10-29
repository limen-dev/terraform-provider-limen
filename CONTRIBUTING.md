# Contributing

## Layout

| Path | What |
|---|---|
| `main.go` | The plugin server (protocol 6). |
| `internal/provider` | Provider configuration, the document resource (one implementation for every writable kind, configured by `kinds.go`), data sources, diagnostics. Acceptance tests are `*_test.go` here. |
| `internal/client` | Connection to Limen: mTLS, bot login and renewal, the Limen CLI's saved login, retries, error explanations. |
| `internal/specjson` | `spec` between Terraform values and canonical JSON. |
| `internal/fakelimen` | In-memory stand-in for `limen.manage.v1` and the Auth service, for tests and `make fake`. |
| `internal/limenpb` | Generated from `proto/` (`make proto`). Don't edit. |
| `proto/limen/manage/v1` | Copied verbatim from limen-dev/limen. |
| `proto/limen/v1/auth.proto` | The three Auth methods the provider calls, with Limen's names and field numbers. |
| `examples/`, `templates/` | Sources of `docs/` (`make docs`). Don't edit `docs/` by hand. |

## The API contract

`limen.manage.v1` belongs to Limen. When it changes there:

```sh
scripts/sync-proto.sh ../limen origin/main   # copy, regenerate, and check the Auth subset
```

`scripts/sync-proto.sh --check` only compares. CI can't read the private Limen repository yet, so run it before a release.

## Tests

- Unit tests: `make test`.
- Acceptance tests (`make testacc`, `make testacc-tofu`) run a real `terraform` or `tofu` against `fakelimen`. They need no control plane and run in CI on the oldest and newest supported CLIs.
- Anything checked only against `fakelimen` is **unverified** against Limen. Say so in pull requests and docs.

`fakelimen` follows ADR-0042 where the provider can observe it (etags, ownership, grants, profiles, request ids, dry runs, held changes, error details). When the provider depends on new server behavior, model it in `fakelimen` first, with a test.

## Trying a local build

```sh
make install                     # builds bin/terraform-provider-limen
make fake &                      # in-memory Limen; writes .fakelimen/env.sh
source .fakelimen/env.sh
cat > dev.tfrc <<EOT
provider_installation {
  dev_overrides { "limen-dev/limen" = "$PWD/bin" }
  direct {}
}
EOT
export TF_CLI_CONFIG_FILE=$PWD/dev.tfrc
```

Then `terraform plan` (or `tofu plan`) in any configuration that uses `limen-dev/limen` with `default_source` set. The environment logs in as the bot `payments-terraform` (namespace `payments`, names `payments-*`, profile `payments-postgres`). `go run ./tools/fakelimen -approve 5s` approves held changes after 5 seconds.

## Commits and pull requests

- Conventional commits (`feat:`, `fix:`, `docs:`, `chore:`): release-please builds the changelog and the version from them.
- Small, focused commits; stage specific files.
- Open pull requests as drafts. CI starts when one is marked ready for review.
- New dependency: say why in the pull request, and check its maintenance and advisories.
