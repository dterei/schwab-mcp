# Security Policy

## Supported Versions

There are no releases yet. Fixes go to the `main` branch.

## Reporting a Vulnerability

Please email code@davidterei.com rather than opening a public issue.

This project handles brokerage credentials and account data, so reports
about leaking tokens, account numbers or transactions are especially
welcome.

The store that `schwabtx` keeps (`history.db`, see
[docs/store.md](docs/store.md)) holds full account numbers and your whole
transaction history, and so do its backups and the exports made with
`-raw` or `-show-accounts`; each is made readable only by you on Linux and
macOS. A way to read any of them that the owner did not intend, or any
command output, view or export that shows a full account number without
those flags, is a vulnerability.
