# Operator Controls and MVP Gate

Status: CHANGES_REQUESTED — checkpoint 2B; switching implementer for reliability
Current checkpoint: 2B
Base: 2b2de0b8238d3c0a1a7e589eda42ad9a18214e99
Last approved: 99a0ab2

## Orchestration recovery — 2026-09-27

User approved checkpoints 1–4. Original child inherited Astra due to configuration;
user corrected routing. GLM session ses_f1cd83872ffeC5M54V5Qwohfr0 used verified
opencode-go/glm-5.3-flash#default through a64b645. Do not resume it for this task.
No model overrides or assistant config edits. Its recovered partial work is now
included in checkpoint 1's approved commits.
Preserve/exclude all four untracked hello-world*.md diagnostic files from commits.

2026-09-28 fallback: switch to next-priority kimi-implementer. GLM required repeated
corrections for explicit acceptance requirements, then CP2B changed approved retry
fixtures outside scope (three independently reproduced failures), delivered a
missing-argument CLI panic plus contradictory snapshot/overflow claims, and put
CP2B progress in the CP2A handoff section. This is a demonstrated reliability/scope
limitation, not model preference. Resume from Git + current handoff; retain useful
implementation, restore unintended regressions, then review corrections. Kimi's
model was verified earlier as opencode-go/kimi-k2.7-code#default; verify new session.

## Scope and decisions (approved; architect-owned)

Implement the remaining phase-3 operator/runtime slice in sequential checkpoints.
Keep this single ignored handoff current; do not force-add docs. Product changes
use small Verb + x commits. No PR/push without request. Initial tracked tree clean.

- Preserve standard Go/net/http/database/sql, handwritten SQL, explicit migrations,
  fixed Together endpoint/model, one local provider call, existing finite limits,
  conservative attempt accounting, and disabled seed policies. No public admin or
  generation routes, generic service layer, provider registry, paid ordinary tests,
  production deployment, discovery, CI or retention infrastructure.
- Add a separate trusted `cmd/admin` CLI using database credentials only. Prefer
  explicit UUID targets and bounded strict JSON files for persona/policy inputs.
  Commands cover persona create/select, policy set, agent pause/resume, job list/
  inspect/retry, usage/status, account disable, post/reply remove. Read operations
  return bounded structured output; errors/logs never echo secrets or content.
- Individual pause/resume updates settings. Global operations follow the existing
  architecture: transactionally update existing settings in deterministic lock
  order, not a persistent fleet-wide latch. Explicit `resume --all` enables all
  valid configured non-disabled agents, including initially disabled seeds; report
  the affected count and document this prominently. No implicit resume on startup.
- Strengthen pause across immediate resume: increment a persisted per-agent pause
  revision under settings locks, snapshot it on spend admission, and require an
  exact match at publication. Old admitted calls may settle but cannot publish.
  This does not refund remote cost or cancel all pending jobs. Existing lease and
  source checks remain authoritative. Append migration 008 for required fields;
  migrations 001–007 remain immutable; move future discovery numbering to 009.
- Persona versions are immutable; selection affects future jobs only. Policy edits
  preserve last publication/history and never resample today's consumed allowance.
  Scheduling-field edits conservatively retire the remaining current-day schedule
  and retain a date ceiling across timezone changes; new opportunities start on a
  subsequent eligible local day. Non-scheduling edits preserve schedule progress.
- Retry only eligible failed jobs with original identity/expiry/attempt history;
  honor max attempts, invalid-output limits and provider retry hints. Never reopen
  succeeded/skipped/cancelled jobs, erase usage, or bypass safety/accounting stops.
  Explicit retry may acknowledge repaired credentials/configuration failures;
  distinguish that from an automatic service restart, which must not clear stops.
- Account removal means irreversible soft disable plus session revocation, not
  hard erasure of history or all authored content. Agent disable prevents its
  publication. Content removal is a separate soft-delete with existing source-lock
  barriers and retained provenance. Do not claim account disable is equivalent to
  deleting every source/actor's content; no browser authorization bypass.
- Add `worker serve`: periodic bounded scheduling and execution, cancellation-aware
  waits, one active call, no catch-up bursts. Keep check/schedule/execute commands.
  Fixed polling/backoff/timeouts stay code constants. Only execute/serve read the
  provider secret. Add worker-only listener env with loopback default.
- Infrastructure HTTP exposes only health/readiness on the separate listener;
  no provider calls from probes or private diagnostics in responses. Readiness
  incorporates database/schema and service progress/degradation. Fatal provider
  credentials/config/accounting errors stop execution safely; transient outages
  remain bounded and observable. API operation is independent of worker readiness.
- Safe structured summaries plus bounded admin status/usage provide queue age,
  heartbeat/progress, failures/skips/retries and conservative charged tokens. No
  raw prompts/output/provider errors, fabricated billing dollars, or new monitoring
  stack. Shutdown stops scheduling/claims, cancels calls, settles uncertainty within
  the existing cleanup budget, joins work, shuts down HTTP and closes DB last.
- Live evaluation is a separate approval gate. No paid calls, credential access or
  live agent enabling is authorized by implementation approval. Before evaluation,
  agree isolated dataset, credentials delivery, call/token and monetary caps,
  current provider pricing and scoring/stop criteria. Keep live/MVP checkboxes open
  until evidence exists; fake tests alone cannot complete them.

## Checkpoint 1 — Settings controls and durable pause barrier

Objective: deliver the trusted persistence contract before CLI/runtime integration.
Write scope: app generation contracts; postgres persona/settings/admission/
publication files and tests; append migration 008 and migration tests; this handoff.
Dependencies: user approval; existing publication/settings lock contract.

### Acceptance
- [x] Immutable version creation/selection and strict policy updates are atomic.
- [x] Policy edits cannot reroll schedules or erase publication/quota history.
- [x] Single/all pause/resume serialize with spend/publication without lock inversions.
- [x] A pre-pause admitted result cannot publish after pause or pause/resume.
- [x] Upgrade preserves old attempts/provenance and fails closed for stale authority.

Validation: focused real-PostgreSQL migration, control, admission/publication and
race tests; app tests. Review schema compatibility and all lock interactions.

### Implementer
Agent: glm-implementer (first priority)
Status: READY_FOR_REVIEW
Commits: rework after CHANGES_REQUESTED: 1412671, 784ea7c, 03fb6a1 (test-only
pointee-mutation correction; correction diff: 47b6def..03fb6a1; checkpoint base
diff now 2b2de0b..03fb6a1)
Validation: corrections only. Settlement revision snapshot: lock-wait regression
TestGenerationSettlementRevisionSnapshotDuringBudgetWait now mutates the ORIGINAL
shared pointee (`*attempt.PauseRevision = 9`) after settlement blocks on the
budget lock — a snapshot that copied only the pointer fails this too.
Demonstrated: with the fix temporarily reverted the test fails (resource
conflict, /tmp/opencode/cp1-rework-nosnapshot.log); with the fix it passes
(/tmp/opencode/cp1-rework-focused2.log, 1 pass/0 fail) and under -race with the
pause suite (/tmp/opencode/cp1-rework-race2.log, no races). No broader checks or
smoke rerun per review scope. Test-only correction 03fb6a1 on top of earlier
rework 1412671 + 784ea7c (correction diff 47b6def..03fb6a1; checkpoint base
diff 2b2de0b..03fb6a1). F2/F3 results from the previous pass unchanged
(/tmp/opencode/cp1-rework-focused.log, -race cp1-rework-race.log, smoke
cp1-rework-smoke.log).
Deviations: none. hello-world*.md diagnostics preserved untracked, never staged.
Blockers: none

### Architect review
Status: APPROVED through 03fb6a1 on 2026-09-28
Reviewed full checkpoint and correction diffs; verified normal/race logs and smoke
(PostgreSQL 18.6 focused runs, native 16.15 smoke). Settings/account lock order,
nullable legacy revision, immutable attempt authority and schedule retirement meet
scope. Findings resolved: snapshot shared revision before waiting; actual fresh
revision-1 publication after single/all resume; migration smoke. Original-pointee
regression demonstrably fails without fix and passes normal/race with it. No open
finding. Complete base diff whitespace check passes. Final full gate remains CP4.

## Checkpoint 2A — Trusted retry and removal persistence

Objective: complete the mutation boundary before adding operator queries/CLI.
This is a review subdivision of approved checkpoint 2, not added feature scope.
Write scope: narrow app/postgres retry, account disable and post/reply removal
operations/tests; narrow worker explicit-retry integration/tests if necessary.
No CLI, inspection queries, service loop, new provider/model or seed changes.
Dependencies: checkpoint 1 APPROVED.

### Acceptance
- [x] Eligible failed-job retry retains job/expiry/lease/attempt/quota identities;
      uses fresh locked DB time, retained Retry-After, max-attempt and invalid-output
      limits; no final/exhausted/unsafe/unsupported-accounting reopening.
- [x] Retry rechecks source/account/settings/current policy with existing lock order;
      it grants no immediate spend/publication authority and refunds nothing.
- [x] Explicit retry after repaired credentials/config can execute with new budget;
      ordinary claim/restart cannot silently turn a fatal outcome into a new call.
      Existing availability-after-finish acknowledgement may be reused only if
      proven unambiguous. Do not erase attempts. Flag any schema/authority ambiguity
      (reasoning in the implementer notes; ambiguity flagged for review).
- [x] Trusted account disable is idempotent, revokes sessions and stops the disabled
      agent's publication; existing content remains until separately removed.
- [x] Trusted soft-delete post/reply preserves existing locks/source invalidation,
      provenance and human/session authorization. No arbitrary-author HTTP bypass.

Validation: real-PG normal/race tests for retry eligibility, concurrency/rollback,
retained accounting, source deletion/publication waits, disable/auth interactions;
focused worker test proving explicit retry behavior. Run smoke if production
HTTP/session behavior changes. No full suite duplication before final gate.

### Implementer
Agent: glm-implementer
Status: APPROVED
Commits: through 99a0ab2
Validation: focused real-PG normal/race retry suite (cp2a-r5-retry.log,
cp2a-r5-race.log); auth and smoke PASS. See architect review.
Deviations: none
Blockers: none

### Architect review
Status: APPROVED through 99a0ab2 on 2026-09-28
Reviewed full initial checkpoint diff plus each correction range and validation
logs. Current policy revalidation, root-before-child locks, retained usage/hints,
failure eligibility and final-state protection accepted. Worker acknowledgement
relaxes only credential/config stops; unsupported accounting remains stopped.
Real-store RetryGeneration -> Execute -> fresh reservation/publication verifies
the integration. Trusted account disable/session revocation and soft deletion
preserve authority/source barriers and provenance. Normal/race, auth and smoke
PASS; final root-order/terminal-reason/hint corrections passed focused real-PG
normal/race (cp2a-r5-{retry,race}.log). No outstanding findings. No full final
validation claim yet; complete implementation gate remains CP4.

## Checkpoint 2B — Trusted CLI and inspection integration

Objective: expose bounded operator operations without HTTP authority changes.
Write scope: cmd/admin; narrow app/postgres operator methods and tests; existing
content removal helpers only as needed; README command synopsis. No worker loop.
Dependencies: checkpoint 2A APPROVED.

Implementation contract: use existing trusted mutations unchanged. A separate
cmd/admin uses config.Database, strict command/flag parsing, a bounded command
deadline, UUID targets and JSON output. File payloads are size-limited before
decoding; reject unknown/duplicate fields, missing fields and trailing input.
Do not echo persona text/file bodies or raw DB/provider errors. Persona create
and select may be separate commands (existing atomic create+select also available).
Job lists use capped keyset pages (stable created_at/id ties), optional exact
agent/status filters; attempt inspection stays bounded. Operator-only cursors need
not use browser HMAC signing. Daily UTC usage must reuse admission's conservative
cost rules, include reserved/unknown calls, distinguish known usage from charged
tokens, and fail closed on overflow. Status reports bounded queue/failure/usage
information, not provider billing guarantees. Read multi-query reports through a
consistent snapshot. Add only indexes justified by these actual queries; flag any
additional migration need before writing it. README commands stay concise, detailed
operator semantics belong in ignored docs. Preserve disabled seed defaults and
explicitly document resume-all includes initially disabled valid agents.

### Acceptance
- [ ] Strict bounded persona/policy input and all listed admin commands work.
- [ ] Eligible retries retain publication identity, expiry, quota and charged usage;
      exhausted, final, unsafe and unsupported-accounting work cannot be reopened.
- [ ] Trusted account disable revokes sessions; content remove blocks in-flight
      source publication and retains provenance; human authorization is unchanged.
- [ ] Job/attempt pages and UTC-day usage/status are bounded and deterministic;
      charged usage matches admission accounting, including unknown calls.
- [ ] Invalid arguments and failed/ambiguous transactions report safely.

Validation: CLI parsing/output tests; real-PostgreSQL concurrency, retry eligibility,
removal/source races, usage boundaries, auth regressions; targeted race checks.
Checkpoint review focuses on trusted authority, retry limits and query bounds.

### Implementer
Agent: kimi-implementer (fallback from GLM; see recovery note)
Status: READY_FOR_REVIEW
Commits: base 99a0ab2; correction diff 99a0ab2..HEAD:
  - 3350b19 Restore generation_retry_test.go to approved checkpoint 2A
  - 16c1091 Harden admin persona/policy input with exact-field strict JSON and safe errors
  - 33392c3 Fix admin reports to use one snapshot, checked totals, truthful truncation and due timestamps
  - 7a957ca Harden admin CLI parsing and add isolated-PG dispatch tests
  - ee106a3 Fix README admin command synopsis and file shape notes
Validation: restored retry suite normal/race PASS
  (/tmp/opencode/cp2b-retry-normal.log, /tmp/opencode/cp2b-retry-race.log);
  admin app/postgres/CLI normal/race PASS
  (/tmp/opencode/cp2b-admin-normal.log, /tmp/opencode/cp2b-admin-race.log);
  smoke PASS (/tmp/opencode/cp2b-smoke.log) after command-delivery changes.
  Focused checks cover exact-field persona/policy strictness, case-alias/duplicate/
  sentinel non-echo, command-scoped flags, exact integer parsing, missing-arg paths,
  one-snapshot status with DB-time UTC day, checked fleet usage totals, truthful
  attempt truncation, equal-time UUID tie traversal, future-work oldest_due handling,
  and real isolated-PG CLI dispatch for persona/policy, pause/resume single/all,
  reports, retry success/denial, account disable and post/reply removal.
Deviations: none
Blockers: none

### Architect review
Status: CHANGES_REQUESTED
Reviewed complete 99a0ab2..a64b645 diff and focused logs. Blocking findings:
1. Restore internal/postgres/generation_retry_test.go exactly to approved 99a0ab2;
   CP2B's three unrelated fixture rewrites violate immutable attempt constraints.
   Independently reproduced failures: accounting_retained, skip_attempt_in_failed_row,
   attempt_limit. Log /tmp/opencode/cp2b-architect-regression.log. Rerun that suite.
2. CLI parse safety: `admin agent pause`/resume with no target panics (confirmed,
   /tmp/opencode/cp2b-architect-cli.log). Flags are not command-scoped: persona create
   A FILE --agent B silently overrides its target; status accepts irrelevant flags.
   fmt.Sscanf accepts integer prefixes such as 3junk. Use simple explicit command
   arity/flag rules, exact bounded numeric conversion, reject duplicates/irrelevant
   flags and target overrides. Test missing args for every command and real dispatch,
   not merely parse happy paths. No framework/command registry abstraction needed.
3. Strict/safe payload boundary: persona JSON accepts case-alias fields because
   DisallowUnknownFields is case-insensitive; Version/version can override each
   other. Decode errors can echo unknown field names or malformed created_at text;
   policy errors likewise can carry user input into stderr. Enforce exact fields,
   duplicate/null/trailing rejection, actual Persona validation (avoid partial
   duplicated validators), cap both file reads at their real schema limits, and
   sanitize all command-boundary errors (including unknown flags). Add sentinel
   secret/control-text non-echo tests plus escaped duplicate/case aliases. Keep
   input creation timestamp contract if retained and document the schema clearly.
4. Reporting correctness: GenerationStatus calls Store.CheckGenerationConfiguration
   inside another readSnapshot, then DayGenerationUsage AFTER that snapshot, so it
   is neither one snapshot nor safe with pool max=1. Refactor narrow Queries helpers
   so the entire report uses one transaction and DB-time UTC day. `map[any]` keyed
   by oldest timestamps loses one field when equal; scan nullable typed timestamps
   directly. oldest_due must describe actually due, unexpired queued work (not
   future availability). Define/document which timestamp represents backlog age.
   Add same-timestamp, future-job and single-connection/snapshot regression tests.
5. Usage overflow: per-agent SQL sum scan is checked but Go fleet total += can
   overflow silently. Check all aggregate additions or use checked SQL totals.
   Verify multiple agents, unknown/reserved/unsupported, day/filter boundaries and
   deliberately overflowing fleet totals. Do not imply charged tokens are dollars.
6. Inspection/list guarantees: Complete is never set or emitted; >max attempts
   return the first four silently. Return an explicit truthful truncation/completeness
   indicator (or explicit safe overflow error), and test >cap rather than 2<3 rows.
   Ties test currently uses distinct timestamps and rejects equal ones; exercise
   equal-time UUID tie traversal without gaps/duplicates plus mixed-agent filters.
   Cursor input/length/zero-time/ID bounds must be validated before SQL. Cursor can
   be computed from the already-returned last job; remove its redundant DB reread.
7. Actual command integration/documentation is missing: add real isolated-PG CLI
   dispatch tests for persona/policy, single/all pause/resume, reports, retry and
   removal (no real provider). Verify success/denial JSON and no secret reading.
   README repeats --agent, uses shell pipes as command alternatives, and does not
   say how to build/run admin or document file shape. Fix concise commands; put
   detailed input/soft-disable/retired-schedule semantics in ignored operator docs.

No new migration required for correctness at MVP volume; no list-index expansion
without measured need. Batched job hydration is preferable to 64 individual reads
but secondary to the correctness findings. Keep rework within 2B, no mutation
redesign or service work. Run focused normal/race tests incl restored retry suite
and real CLI integration, make smoke for command delivery changes. Final full
make check remains CP4. Clean up implementer handoff fields: CP2B progress was
mistakenly inserted into CP2A; move/condense it in place, preserve approvals.

## Checkpoint 3 — Continuous worker and operational signals

Objective: autonomous scheduling/execution with safe lifecycle and degradation.
Write scope: internal/worker, cmd/worker, internal/config, associated tests,
.env.example and concise README service commands. HTTP/JSON remains internal/api
(a separate infrastructure handler, not public API routes).
Dependencies: checkpoint 2B APPROVED; reuse reviewed store operations.

### Acceptance
- [ ] Serve runs bounded passes automatically, never overlapping local calls.
- [ ] Probes expose infrastructure status only, no provider call or private data.
- [ ] Transient/fatal failures have explicit bounded behavior and safe reporting;
      restarting does not silently acknowledge a fatal durable failure.
- [ ] Worker secrets/listener are role-specific; check/schedule/admin remain key-free.
- [ ] Bind failure, DB outage, cancellation, in-flight shutdown and crash recovery
      respect ownership, cleanup deadlines and conservative reservations.

Validation: deterministic fake provider loop/lifecycle/race tests, config tests,
real-store shutdown/recovery cases; focused smoke for startup changes. Review
readiness semantics, resource ordering and service-to-executor integration.

### Implementer
Agent: glm-implementer
Status: PENDING
Commits: none
Validation: not run
Deviations: none
Blockers: checkpoint 2B

### Architect review
Status: PENDING
Findings: none

## Checkpoint 4 — Integration evidence and implementation final gate

Objective: prove operator controls plus autonomous service using fake providers.
Write scope: test-only command harness, smoke fixtures/scripts and runner tests as
needed, README and ignored docs/checklist. No production fake-provider selector.
Dependencies: checkpoint 3 APPROVED.

### Acceptance
- [ ] Built admin/service + real API/PG demonstrate scheduled publication and
      comment/repost/quote responses without manually invoking execute each time.
- [ ] Pause/resume/delete during calls, unknown usage, shared budget/chain caps,
      failure isolation and restarts are verified; no duplicate publications.
- [ ] HTTP reads/writes remain usable during provider failure; data/provenance
      survive restart. Ordinary tests cannot use ambient real credentials.
- [ ] Docs describe commands, removal/global-resume semantics, finite limits,
      readiness, safe monitoring and remaining live gate. Only verified items checked.

Validation: implementer focused integration checks. Architect final full pass:
`make check`, `make smoke`, `python3 scripts/test_runner.py` if runner changed,
formatting and complete base diff review. Dependency startup/cleanup failure is a
blocker, never replaced with skipped PostgreSQL tests. Save verbose logs under
/tmp/opencode. Software can be PR-ready while live gate remains explicitly pending.

### Implementer
Agent: glm-implementer
Status: PENDING
Commits: none
Validation: not run
Deviations: none
Blockers: checkpoint 3

### Architect review
Status: PENDING
Findings: none

## Checkpoint 5 — Separately authorized live evaluation and MVP gate

Objective: evaluate persona voice, relevance, repetition, safety, latency and token
accounting under approved finite spending; tune only from recorded evidence.
Write scope: ignored evaluation report/checklist; explicit policy tuning only after
approval. Any required product correction gets a reviewed implementation task.
Dependencies: checkpoint 4 APPROVED and explicit user authorization of evaluation
environment, caps, credentials, acceptance rubric and enabling selected agents.

### Acceptance
- [ ] Budget-capped live report records results and failure/stop conditions honestly.
- [ ] Scheduled and human comment/repost/quote flows publish/persist autonomously
      without an approval queue, within approved policy and spending.
- [ ] Architect/user accept quality/safety evidence; policies end in agreed state.

Validation: approved live protocol only; no claims of comprehensive semantic safety,
exactly-once provider billing or guaranteed remote cancellation.

### Implementer
Agent: not assigned (evaluation needs approval)
Status: PENDING
Commits: none
Validation: not run
Deviations: none
Blockers: explicit live-evaluation authorization and caps

### Architect review
Status: PENDING
Findings: none
