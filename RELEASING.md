# Releasing and publishing

Releases are automatic once publishing is set up: conventional commits on `main` make release-please open a release pull request; merging it tags `vX.Y.Z`, and `release.yml` builds, signs, and publishes the GitHub release. Both registries read the GitHub release.

Before 1.0, `feat` bumps the minor version and `fix` the patch.

## One-time setup (owner)

The registries only publish **public** repositories named `terraform-provider-<name>`. Do these in order.

1. **Signing key.** Create a key used only for releases. The Terraform Registry accepts RSA keys, so use RSA 4096:
   ```sh
   gpg --quick-generate-key "Limen releases <releases@limen.dev>" rsa4096 sign 3y
   gpg --armor --export-secret-keys <FINGERPRINT> > private.asc   # for the secret, then delete it
   gpg --armor --export <FINGERPRINT> > public.asc
   ```
   Keep a revocation certificate somewhere safe.
2. **Repository secrets** (Settings → Secrets and variables → Actions): `GPG_PRIVATE_KEY` (the contents of `private.asc`) and `GPG_PASSPHRASE`. Consider a `release` environment with required reviewers and move the secrets there.
3. **Make the repository public** (Settings → General → Danger zone). Check the history first: `git log -p` should contain nothing private.
4. **Terraform Registry.** Sign in at <https://registry.terraform.io> with GitHub, with access to the `limen-dev` organization. Under *User settings → Signing keys*, add `public.asc` for the `limen-dev` namespace. Then *Publish → Provider*, choose `limen-dev/terraform-provider-limen`. The registry adds a webhook and picks up every later release by itself.
5. **OpenTofu Registry.** Open two issues in [opentofu/registry](https://github.com/opentofu/registry/issues/new/choose): *Submit new Provider* (repository `limen-dev/terraform-provider-limen`), and *Submit new Provider Signing Key* (namespace `limen-dev`, the contents of `public.asc`). Once merged, the registry syncs new releases by itself.
6. **First release.** Merge the release pull request release-please opened (or push a `v0.1.0` tag). Check that the release has the zips, `SHA256SUMS`, `SHA256SUMS.sig`, and `manifest.json`, then that both registries show the version.

## Each release

1. Run `scripts/sync-proto.sh --check ../limen origin/main`; if it fails, sync and release the change first.
2. Check the compatibility table in `templates/index.md.tmpl` (and so `docs/index.md`) and the README against the Limen release it supports.
3. Merge the release pull request.

## If something goes wrong

- A bad release: publish a fixed patch release. Don't delete or re-tag a published version; registries and lock files keep its checksums.
- A leaked signing key: revoke it, remove it from both registries, rotate the secrets, and release again with a new key.
