# Security policy

Security fixes are developed against `main`. Knotra is pre-1.0; there is no supported historical
release series or promised response deadline yet.

## Report a vulnerability

Use the repository's **Security → Report a vulnerability** private reporting channel when available.
If it is unavailable, contact a maintainer privately before disclosing exploit details. Do not post
credentials, private pipeline content or a working exploit in a public issue.

Include the revision, affected component, prerequisites, minimal reproduction, expected boundary and
observed behavior. Redact tokens and personal data. Coordinate public disclosure with the
maintainers after a fix is available.

## Deployment boundaries

The current engine is intended for a trusted operator on one host. Remote API access requires TLS
and a bearer token. Docker sandbox permissions, image selection, network allowlists and secret
grants are engine configuration. A shared public service for independent users requires additional
isolation and access controls. See [running](docs/running.md) and
[adapter guarantees](internal/adapters/README.md) for the supported boundaries.
