# Security policy

## Supported versions

| Edition | Supported |
|---|---|
| Watched 2 (`:2`, `:latest`, `main`) | Yes |
| Watched 1 lite (`:1`, `v1` branch) | Security fixes only |

## Reporting a vulnerability

Please **don't open a public issue** for security problems.

Report privately through GitHub instead: go to the
[Security tab](https://github.com/Dev-APkash/watched/security) and choose
**Report a vulnerability**. Include the version or image tag, what an attacker
could do, and the steps to reproduce it.

You should hear back within a week. Once a fix is released, the advisory is
published with credit to you, unless you'd rather stay anonymous.

## Scope

Watched is meant to run on a home network or behind your own reverse proxy and
has no login of its own. Things that are in scope:

- Leaking the Jellyfin API key, Trakt tokens or other settings
- Reading or changing files outside `DATA_DIR`
- Cross-site scripting through titles, notes or other library data
- Making Watched send requests somewhere it shouldn't (SSRF)

Exposing Watched to the public internet without authentication in front of it
is a deployment choice, not a vulnerability.
