CREATE TABLE IF NOT EXISTS decision_evaluations (
 workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 id uuid NOT NULL,
 request_hash text NOT NULL,
 submitted_by text NOT NULL,
 payload jsonb,
 review_status text NOT NULL DEFAULT 'unreviewed' CHECK(review_status IN ('unreviewed','agent_generated','reviewed','withdrawn')),
 review jsonb,
 reviewed_by text,
 reviewed_at timestamptz,
 permission_granted_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(workspace_id,id)
);
CREATE INDEX IF NOT EXISTS decision_evaluations_expiry ON decision_evaluations(expires_at) WHERE payload IS NOT NULL;

CREATE TABLE IF NOT EXISTS decision_feedback_policies (
 workspace_id uuid PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
 agent_candidates_enabled boolean NOT NULL DEFAULT false,
 updated_by text NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);
