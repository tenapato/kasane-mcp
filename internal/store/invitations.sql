CREATE TABLE IF NOT EXISTS waitlist(
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 email text UNIQUE NOT NULL,
 name text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS invitations(
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 email text NOT NULL,
 token_hash text UNIQUE NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 accepted_at timestamptz,
 revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS invitations_email_idx ON invitations(email);
