ALTER TABLE owners ADD COLUMN IF NOT EXISTS email text;
CREATE UNIQUE INDEX IF NOT EXISTS owners_email_unique ON owners(email) WHERE email IS NOT NULL;
ALTER TABLE owners ADD COLUMN IF NOT EXISTS role text NOT NULL DEFAULT 'user' CHECK(role IN ('admin','user'));
-- Only accounts that predate the multi-user migration are instance admins.
UPDATE owners SET role='admin';
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS owner_username text REFERENCES owners(username);
UPDATE workspaces SET owner_username=(SELECT username FROM owners ORDER BY username LIMIT 1) WHERE owner_username IS NULL;
CREATE INDEX IF NOT EXISTS workspaces_owner ON workspaces(owner_username);
