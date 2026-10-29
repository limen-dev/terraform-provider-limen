# Security

Report vulnerabilities privately through GitHub's [private vulnerability reporting](https://github.com/limen-dev/terraform-provider-limen/security/advisories/new), not in an issue. For vulnerabilities in Limen itself, report to [limen-dev/limen](https://github.com/limen-dev/limen/security).

What the provider is careful about:
- It never logs or stores a workload token. Tokens and private keys stay in memory; the bot's key is generated in the process and never written.
- It never needs `limen:admin`. A bot writer's grant bounds what any configuration can do, whatever the configuration says.
- `client_certificate.key_pem` and `bot.token` are sensitive, so Terraform hides them, but they are still in the plan file and the configuration. Prefer `token_env` and `token_file`.
- Pull request runs can't log in as a bot (ADR-0019); plan them with `offline = true`.
