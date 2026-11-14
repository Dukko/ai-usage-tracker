# Security

This app stores API keys and OAuth tokens for your AI accounts, so security reports are very welcome.

**Please don't open a public issue for a vulnerability.** Use GitHub's private
[security advisory form](https://github.com/Dukko/ai-usage-tracker/security/advisories/new) instead.
You'll get a reply within a week.

## How credentials are protected

- Stored credentials are encrypted with AES-256-GCM. The key comes from `TRACKER_SECRET_KEY` or a random
  `secret.key` in the data directory. Anyone with both the database and that key can read them, so back them
  up separately and keep the data volume private.
- Credentials are never sent to the browser. Pages and `/api/accounts` show usage numbers only.
- Passwords are hashed with PBKDF2-SHA256 (600,000 rounds). Session, API and push tokens are stored as SHA-256
  hashes.
- Sign-in is rate limited per IP. Pages send a strict Content-Security-Policy, can't be framed, and forms reject
  cross-site requests.

## Supported versions

Only the latest release gets fixes.
