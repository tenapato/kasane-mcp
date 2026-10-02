# Agent evaluations for Kaname training

Agents can submit relevance judgements through `kasane_relevance_evaluate`. Kasane
runs Kaname against the selected saved practices, then stores the exact task,
paths, optional context, practice text/revisions, prediction, agent labels and
rationales in a workspace evaluation queue. This is an explicit write operation.
Ordinary `kasane_relevance`, dashboard checks, searches and profile calls still
retain no training snapshots.

With automatic contributions enabled by the workspace owner, submissions become
**agent-generated training candidates immediately**, without per-example human
approval. They preserve `agent_generated` label provenance and can be used only
in training, never calibration or held-out quality evaluation. They cannot change
practices, start training, or update serving weights. Without workspace opt-in,
submissions remain unreviewed. Optional human review is a separate path.

## Enable capture

Set `DECISION_FEEDBACK_RETENTION_DAYS=30` on the **Kasane MCP/backend** and redeploy.
Then open **Kaname → Training candidates** for the selected workspace, consent to
the listed content categories, and click **Enable automatic contributions**.
This is a one-time workspace setting; MCP keys cannot grant it themselves.
The default retention value `0` disables new submissions; accepted values are
1–365 days. Kaname's
own configuration does not change. Each submitted record gets a fixed expiry;
changing the setting affects new records only. Disabling capture does not revoke
existing approvals: withdraw them explicitly when necessary.

Before enabling capture, publish your chosen retention duration and the backup
expiry for the PostgreSQL database. The app cannot control external backups or
exported files. Avoid secrets and full conversations in submitted tasks, context,
and rationales. Collection is limited to explicit submissions with a write key.

Expiry immediately prevents reading/exporting a record. The backend worker purges
expired raw snapshots and reviews at startup and approximately every minute while
running; after downtime it catches up on restart. Withdrawal immediately purges
raw content. Minimal IDs, timestamps and withdrawal metadata remain so a fresh
permission registry can reject previously exported contributions. Workspace
removal cascades to these records. Keep downloaded exports private and delete
expired/withdrawn material under the same published policy. Existing weights are
not instantly unlearned; retire and replace affected bundles as required.

## MCP submission

Read the practices with `kasane_profile`, `kasane_search` or `kasane_get`. Judge
whether each practice applies to the task **independently** of Kaname's output.
Then call:

```json
{
  "name": "kasane_relevance_evaluate",
  "arguments": {
    "workspace_id": "WORKSPACE_UUID",
    "evaluation_id": "NEW_UUID_REUSED_ON_RETRY",
    "source_group": "api-authentication-change",
    "source_origin": "real",
    "agent_model": "YOUR_AGENT_MODEL_ID",
    "task": "Add authentication to the HTTP endpoints",
    "paths": ["src/http.go"],
    "rules": [{"id": "PRACTICE_UUID", "expected_revision": 3}],
    "judgements": [{
      "rule_id": "PRACTICE_UUID",
      "label": "relevant",
      "rationale": "The practice requires authentication checks on these endpoints."
    }]
  }
}
```

Single-workspace keys may omit `workspace_id`. Read-only keys cannot submit.
Select 1–64 practices with their exact `expected_revision` from Kasane, and
provide exactly one judgement per practice. Stale revisions are rejected before
evaluation; re-read and reassess the practice before submitting again. Labels are
`relevant`, `not_relevant`, or `insufficient_context`; rationales are 1–2000 bytes.
`source_origin` is `real`, `authored`, or `synthetic`. Keep related tasks under the
same stable `source_group`; generated examples must not be declared real.
`workspace_id` also becomes the training project group, following Kasane's
workspace-per-project model.

The response includes the evaluation ID, actual decision and saved practice
revisions, `review_status: "agent_generated"`, `ready_for_export: true`, and expiry
when workspace automatic contributions are enabled. Otherwise the record is
`unreviewed` and not exportable. Permission comes from the owner's workspace policy,
never from an agent claiming to have human approval. Same-ID, same-key, same-input retries
return the original result even after a practice changes. Conflicting reuse is
rejected. Evaluation IDs cannot be reused to resurrect withdrawn/expired records.
There are at most 500 retained active evaluations per workspace.

## Owner review and export API

These routes require a signed-in **workspace owner's session cookie**. POSTs
also require the normal `Origin` and `X-CSRF-Token` headers. MCP bearer keys cannot
call them. The Kaname dashboard supports the workspace contribution setting, recent
candidate downloads and withdrawal. Manual human review uses the API.

- `GET` or `POST /api/v1/workspaces/{workspace}/decision/training-policy` reads or
  sets `{"agent_candidates_enabled": true}`. Setting false atomically withdraws
  existing automatic candidates and purges their raw snapshots.
- `GET /api/v1/workspaces/{workspace}/decision/evaluations?offset=0` returns ten
  records per page, including original snapshots and proposed labels.
- `POST /api/v1/workspaces/{workspace}/decision/evaluations/{id}/review` approves
  one pending record with independently checked/corrected labels and explicit
  consent covering task, paths, rule text and optional context/code.
- `POST /api/v1/workspaces/{workspace}/decision/evaluations/{id}/withdraw` revokes
  eligibility and purges its raw snapshot/review. Use this to reject a proposal too.
- `GET /api/v1/workspaces/{workspace}/decision/evaluations/{id}/export` downloads a
  ZIP for an unexpired automatic candidate or a human-approved record.
- `GET /api/v1/workspaces/{workspace}/decision/permissions` returns the current
  permission registry, including withdrawn/expired approvals.

Optional review body for an **unreviewed** record (automatic candidates do not
require this step; all human-reviewed labels are supplied explicitly):

```json
{
  "shared_training_allowed": true,
  "sensitive_content_reviewed": true,
  "source_origin": "real",
  "judgements": [{
    "rule_id": "PRACTICE_UUID",
    "label": "relevant",
    "rationale": "I checked the original task and this practice applies."
  }]
}
```

Review is immutable once approved: withdraw an incorrect approval and create a
new evaluation rather than silently changing an already exported label. All
exported metadata and practice content use the original decision snapshot, even
if the practice has since changed. `review.json` separately records the agent
model/proposals and any human corrections. Automatic exports have
`human_review: null` and `sensitive_content_reviewed: false`; they do not claim
human screening. The importer still checks for known secret patterns, so export
eligibility does not guarantee dataset admission.

## Feed the existing offline trainer

Extract the ZIP into a private directory (for example, `chmod 700 EXPORT_ROOT`).
Entries have mode 0600. It contains `events.jsonl`, `permissions.json`,
`snapshots/*.json` and `review.json`.

From the Kaname repository:

```sh
scripts/cargo-local run -p decision-tooling --bin decision-feedback -- \
  EXPORT_ROOT EXPORT_ROOT/events.jsonl EXPORT_ROOT/permissions.json admitted.jsonl audit.json
```

Before import and every later training/calibration/evaluation/promotion operation,
refresh `permissions.json` from the workspace permission endpoint. Never treat a
previously downloaded registry as proof of continuing consent. Withdrawn or
expired permissions exclude these examples. If combining multiple workspaces,
combine their current registries without dropping withdrawal entries. The
exported grant expires with the evaluation's retention deadline.

Use the updated Kaname tooling that recognizes `agent_generated` feedback; old
versions reject these exports. Agent candidates retain both their agent model and
original input source origin in provenance and must go in the training split.

The importer verifies snapshots, exact rule revisions, source origin, permission
scope and secret flags. It deduplicates retries and quarantines conflicting
labels. Inspect `audit.json`; admission is not evidence that a model is good.
Pass `--permissions REGISTRY.json` to subsequent Kaname model tooling. Real,
reviewed held-out cases and the existing release gates are still required before
any candidate can be deployed. The currently deployed baseline does not learn
from collecting these records.
