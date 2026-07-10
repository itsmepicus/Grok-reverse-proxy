# Security policy

Do not report vulnerabilities in a public issue. Use GitHub private vulnerability
reporting when it is enabled for the repository.

## Credential handling

- OAuth files, `.env`, runtime state, keys, and account exports are ignored by Git.
- The service never logs request bodies, OAuth tokens, proxy credentials, or client API keys.
- Runtime state is created with owner-only permissions (`0600`).
- The public API requires `GROK_PROXY_API_KEY`. An empty key is accepted only on a loopback listener.
- OAuth and upstream endpoints must use HTTPS unless the explicit local-test override is enabled.

Treat `GROK_STATE_FILE` and every file matched by `GROK_AUTH_FILES` as a password.
If one is exposed, revoke the xAI/Grok session and create a new proxy API key.
