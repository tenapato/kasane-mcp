# Security policy

Run Kasane behind an HTTPS reverse proxy in production. Set `PUBLIC_URL` to the public HTTPS URL so the service uses secure session cookies. Keep the app port bound to localhost unless an upstream firewall protects it.

Agent keys are bearer credentials. Store them in a secret manager, grant read scope unless writes are needed, and revoke them when no longer used. Never put tokens, passwords, or `DATABASE_URL` values in issues, logs, commits, or client-side code.

Report suspected vulnerabilities privately to the project maintainers before opening a public issue. Include the affected version, a clear reproduction, and the smallest proof needed to confirm impact. Do not include live credentials or private memory content.

There is no public registration flow. Create the initial owner with `KASANE_ADMIN_PASSWORD` and `kasane bootstrap --username NAME`.
