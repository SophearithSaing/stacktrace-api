package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func eligibilityPolicy() app.GenerationPolicy {
	p := generationPolicy()
	p.MinSpacingSeconds, p.CooldownSeconds = 1, 1
	p.ResponseMinDelaySeconds, p.ResponseMaxDelaySeconds, p.SourceMaxAgeSeconds = 0, 0, 604800
	p.ReplyCapPerDay, p.ReplyCapPerConversation, p.MaxAgentsPerTrigger = 100, 100, 10
	p.HumanTriggerCapPerWindow = 100
	return p
}

func eligibilityFixture(t *testing.T) (*Store, app.GenerationJob, app.GenerationPolicy) {
	t.Helper()
	store, _, root := generationSetup(t)
	actor, post, _, _ := generationSocial(t, store, root)
	changes := contextSocialChanges(actor, post, app.TriggerHumanPost)
	at := root.CreatedAt.Add(24 * time.Hour)
	changes["created_at"], changes["available_at"], changes["expires_at"] = at, at, at.Add(6*24*time.Hour)
	job := cloneClaimJob(t, store, root.ID, changes)
	p := eligibilityPolicy()
	if _, err := store.InitializeAgentSettings(context.Background(), app.AgentSettings{AgentID: job.AgentID, PersonaVersion: 1, Enabled: true, Policy: p, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	return store, job, p
}

// Tests exercise the preparation and read-only policy helpers directly, not the
// unapproved attempt/spend drafts. No future budget authority is implied.
func assertExecutionAllowed(t *testing.T, store *Store, job app.GenerationJob, p app.GenerationPolicy, last *time.Time, now time.Time, want bool) {
	t.Helper()
	err := store.Transaction(context.Background(), func(q *Queries) error {
		_, _, reason, err := q.lockExecutionEligibility(context.Background(), job)
		if err != nil || reason != "" {
			t.Fatalf("prepare: %q %v", reason, err)
		}
		got, err := q.executionPolicyAllowed(context.Background(), job, p, last, now)
		if err != nil || got != want {
			t.Fatalf("allowed=%v want=%v err=%v", got, want, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func eligibilityHistory(t *testing.T, store *Store, job app.GenerationJob, at time.Time, changes map[string]any) app.GenerationJob {
	t.Helper()
	values := map[string]any{"created_at": at, "available_at": at, "cooldown_key": string(app.NewID())}
	for key, value := range changes {
		values[key] = value
	}
	return cloneClaimJob(t, store, job.ID, values)
}

func TestExecutionEligibilityOwnReservationAndReducedCaps(t *testing.T) {
	for _, quota := range []string{"day", "conversation", "human window", "action"} {
		t.Run(quota, func(t *testing.T) {
			store, job, p := eligibilityFixture(t)
			changes := map[string]any{}
			switch quota {
			case "day":
				p.ReplyCapPerDay = 1
			case "conversation":
				p.ReplyCapPerConversation = 1
			case "human window":
				p.HumanTriggerCapPerWindow = 1
			case "action":
				p.MaxAgentsPerTrigger = 1
				// Another agent may own the same action; unique(agent,key) stays real.
				agent := socialAgent(t, store, "quota_peer", nil)
				changes["agent_id"], changes["trigger_key"] = agent.AgentID, job.TriggerKey
			}
			assertExecutionAllowed(t, store, job, p, nil, job.CreatedAt, true)
			later := eligibilityHistory(t, store, job, job.CreatedAt.Add(2*time.Second), changes)
			assertExecutionAllowed(t, store, job, p, nil, job.CreatedAt.Add(3*time.Second), true)
			// Later terminal work cannot invalidate the earlier legitimate member.
			generationSQL(t, store, `UPDATE generation_jobs SET status='cancelled',reason_code='policy_disabled',finished_at=created_at WHERE id=$1`, later.ID)
			assertExecutionAllowed(t, store, job, p, nil, job.CreatedAt.Add(3*time.Second), true)
			changes["status"], changes["reason_code"], changes["finished_at"] = "failed", "retained", job.CreatedAt
			if quota == "action" {
				changes["agent_id"] = socialAgent(t, store, "earlier_peer", nil).AgentID
			}
			eligibilityHistory(t, store, job, job.CreatedAt.Add(-2*time.Second), changes)
			assertExecutionAllowed(t, store, job, p, nil, job.CreatedAt.Add(3*time.Second), false)
		})
	}
}

func TestExecutionEligibilitySharedQuoteQuotas(t *testing.T) {
	for _, output := range []app.GenerationOutput{app.OutputReply, app.OutputQuote} {
		for _, quota := range []string{"day", "conversation"} {
			t.Run(string(output)+"/"+quota, func(t *testing.T) {
				store, original, p := eligibilityFixture(t)
				// Make the second reservation a different output kind. Both share
				// response quotas regardless of which output is being executed.
				first, second := app.OutputReply, app.OutputQuote
				if output == app.OutputReply {
					first, second = second, first
				}
				// Identity is immutable, so create both test rows at later instants.
				prior := eligibilityHistory(t, store, original, original.CreatedAt.Add(24*time.Hour), map[string]any{"output_kind": first})
				job := eligibilityHistory(t, store, original, prior.CreatedAt.Add(2*time.Second), map[string]any{"output_kind": second})
				if quota == "day" {
					p.ReplyCapPerDay = 1
				} else {
					p.ReplyCapPerConversation = 2
				}
				assertExecutionAllowed(t, store, job, p, nil, job.CreatedAt, false)
				if quota == "day" {
					p.ReplyCapPerDay = 2
				} else {
					p.ReplyCapPerConversation = 3
				}
				assertExecutionAllowed(t, store, job, p, nil, job.CreatedAt, true)
			})
		}
	}
}

func TestExecutionEligibilityStableTiesAndRetainedStatuses(t *testing.T) {
	for _, status := range []string{"pending", "running", "retry_wait", "failed", "cancelled", "skipped"} {
		t.Run(status, func(t *testing.T) {
			store, job, p := eligibilityFixture(t)
			changes := map[string]any{"status": status}
			if status == "running" || status == "retry_wait" {
				changes["lease_version"] = 1
			}
			if status == "running" {
				changes["lease_expires_at"] = job.ExpiresAt
			}
			if status != "running" && status != "pending" {
				changes["reason_code"] = "retained"
			}
			if status == "failed" || status == "cancelled" || status == "skipped" {
				changes["finished_at"] = job.CreatedAt
			}
			peer := eligibilityHistory(t, store, job, job.CreatedAt, changes)
			p.ReplyCapPerDay = 1
			// At identical timestamps spacing itself rejects the later UUID.
			assertExecutionAllowed(t, store, job, p, nil, job.CreatedAt, job.ID < peer.ID)
			assertExecutionAllowed(t, store, peer, p, nil, peer.CreatedAt, peer.ID < job.ID)
		})
	}
}

func TestExecutionEligibilityCooldownAndSpacing(t *testing.T) {
	for _, test := range []struct {
		name     string
		delta    time.Duration
		cooldown bool
		want     bool
	}{
		{"spacing before edge", 59 * time.Second, false, false},
		{"spacing edge", 60 * time.Second, false, true},
		{"spacing after edge", 61 * time.Second, false, true},
		{"cooldown before edge", 59 * time.Second, true, false},
		{"cooldown edge inclusive", 60 * time.Second, true, false},
		{"cooldown after edge", 61 * time.Second, true, true},
		{"later spacing ignored", -time.Second, false, true},
		{"later cooldown ignored", -time.Second, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, job, p := eligibilityFixture(t)
			changes := map[string]any{}
			if test.cooldown {
				p.CooldownSeconds = 60
				changes["cooldown_key"] = job.CooldownKey
			} else {
				p.MinSpacingSeconds = 60
			}
			eligibilityHistory(t, store, job, job.CreatedAt.Add(-test.delta), changes)
			assertExecutionAllowed(t, store, job, p, nil, job.CreatedAt.Add(time.Minute), test.want)
		})
	}
	store, job, p := eligibilityFixture(t)
	p.MinSpacingSeconds = 60
	last := job.CreatedAt.Add(24 * time.Hour)
	assertExecutionAllowed(t, store, job, p, &last, last.Add(59*time.Second), false)
	assertExecutionAllowed(t, store, job, p, &last, last.Add(time.Minute), true)
}

func TestExecutionEligibilityZeroTogglesAndInvalidInputs(t *testing.T) {
	store, job, base := eligibilityFixture(t)
	for name, change := range map[string]func(*app.GenerationPolicy){
		"budget":           func(p *app.GenerationPolicy) { p.DailyTokenBudget = 0 },
		"day":              func(p *app.GenerationPolicy) { p.ReplyCapPerDay = 0 },
		"conversation":     func(p *app.GenerationPolicy) { p.ReplyCapPerConversation = 0 },
		"action":           func(p *app.GenerationPolicy) { p.MaxAgentsPerTrigger = 0 },
		"human":            func(p *app.GenerationPolicy) { p.HumanTriggerCapPerWindow = 0 },
		"probability":      func(p *app.GenerationPolicy) { p.HumanPostProbabilityBPS = 0 },
		"invalid timezone": func(p *app.GenerationPolicy) { p.Timezone = "secret/invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			p := base
			change(&p)
			assertExecutionAllowed(t, store, job, p, nil, job.CreatedAt, false)
		})
	}
	base.HumanPostProbabilityBPS = 1 // Positive probability is never rerolled.
	assertExecutionAllowed(t, store, job, base, nil, job.CreatedAt, true)
	assertExecutionAllowed(t, store, job, base, nil, time.Time{}, false)
	assertExecutionAllowed(t, store, job, base, nil, job.ExpiresAt, false)
	if _, _, _, err := store.lockExecutionEligibility(context.Background(), job); !errors.Is(err, errGenerationTransaction) {
		t.Fatal(err)
	}
	if _, err := store.executionPolicyAllowed(context.Background(), job, base, nil, job.CreatedAt); !errors.Is(err, errGenerationTransaction) {
		t.Fatal(err)
	}
	err := store.Transaction(context.Background(), func(q *Queries) error {
		bad := job
		bad.AgentID = "invalid"
		if ok, err := q.executionPolicyAllowed(context.Background(), bad, base, nil, job.CreatedAt); err != nil || ok {
			t.Fatalf("invalid job: %v %v", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecutionEligibilitySettingsAndPinnedPersona(t *testing.T) {
	for _, invalid := range []string{"none", "missing", "paused", "disabled", "policy", "timezone", "null policy field", "oversize", "schedule", "wrong schedule day", "missing next", "persona"} {
		t.Run(invalid, func(t *testing.T) {
			store, job, _ := eligibilityFixture(t)
			persona, err := store.PersonaByVersion(context.Background(), job.AgentID, 1)
			if err != nil {
				t.Fatal(err)
			}
			persona.Version, persona.Instructions = 2, "New version must not replace pinned persona"
			if err := store.CreatePersona(context.Background(), persona); err != nil {
				t.Fatal(err)
			}
			generationSQL(t, store, `UPDATE agent_settings SET persona_version=2 WHERE agent_id=$1`, job.AgentID)
			switch invalid {
			case "missing":
				generationSQL(t, store, `DELETE FROM agent_settings WHERE agent_id=$1`, job.AgentID)
			case "paused":
				generationSQL(t, store, `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`, job.AgentID)
			case "disabled":
				generationSQL(t, store, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, job.AgentID)
			case "policy":
				generationSQL(t, store, `UPDATE agent_settings SET policy=policy-'timezone' WHERE agent_id=$1`, job.AgentID)
			case "timezone":
				generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{timezone}','"secret/invalid"') WHERE agent_id=$1`, job.AgentID)
			case "null policy field":
				generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{timezone}','null') WHERE agent_id=$1`, job.AgentID)
			case "oversize":
				generationSQL(t, store, `UPDATE agent_settings SET policy=policy||jsonb_build_object('secret',repeat('x',9000)) WHERE agent_id=$1`, job.AgentID)
			case "schedule":
				generationSQL(t, store, `UPDATE agent_settings SET remaining_slots=3,schedule_date='2026-09-21',next_post_at='2026-09-21 12:00Z' WHERE agent_id=$1`, job.AgentID)
			case "wrong schedule day":
				generationSQL(t, store, `UPDATE agent_settings SET remaining_slots=1,schedule_date='2026-09-21',next_post_at='2026-09-22 12:00Z' WHERE agent_id=$1`, job.AgentID)
			case "missing next":
				generationSQL(t, store, `UPDATE agent_settings SET remaining_slots=1,schedule_date='2026-09-21' WHERE agent_id=$1`, job.AgentID)
			case "persona":
				job.PersonaVersion = 3 // No such immutable version.
			}
			err = store.Transaction(context.Background(), func(q *Queries) error {
				p, _, reason, err := q.lockExecutionEligibility(context.Background(), job)
				if invalid == "none" {
					if err != nil || reason != "" || p.Version != 1 {
						t.Fatalf("valid preparation: %q %v", reason, err)
					}
					pinned, err := q.executionPersona(context.Background(), job)
					if err != nil || pinned.Version != 1 || pinned.Instructions != "Discuss practical Go." {
						t.Fatalf("repinned persona: %+v %v", pinned, err)
					}
				} else if invalid == "persona" {
					if !errors.Is(err, app.ErrNotFound) {
						t.Fatalf("missing persona: %v", err)
					}
				} else if err != nil || reason != "policy_disabled" || p.Version != 0 {
					t.Fatalf("unsafe denial: %q %v", reason, err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func saveEligibilityPolicy(t *testing.T, store *Store, agent app.ID, p app.GenerationPolicy) {
	t.Helper()
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	generationSQL(t, store, `UPDATE agent_settings SET policy=$2 WHERE agent_id=$1`, agent, encoded)
}
