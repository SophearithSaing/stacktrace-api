package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestExecutionEligibilitySourceAgeAndProbability(t *testing.T) {
	for _, kind := range []app.GenerationTrigger{app.TriggerHumanPost, app.TriggerReply, app.TriggerRepost, app.TriggerQuote, app.TriggerContinuation} {
		t.Run(string(kind), func(t *testing.T) {
			store, _, root := generationSetup(t)
			actor, post, reply, repost := generationSocial(t, store, root)
			at := root.CreatedAt.Add(24 * time.Hour)
			// The canonical post is old; replies/reposts have their own younger age.
			generationSQL(t, store, `UPDATE replies SET created_at=$2 WHERE id=$1`, reply, at)
			generationSQL(t, store, `UPDATE reposts SET created_at=$2 WHERE id=$1`, repost, at)
			changes := contextSocialChanges(actor, post, kind)
			switch kind {
			case app.TriggerHumanPost:
				generationSQL(t, store, `UPDATE posts SET created_at=$2 WHERE id=$1`, post, at)
			case app.TriggerReply:
				changes["source_reply_id"] = reply
			case app.TriggerRepost:
				changes["source_repost_id"] = repost
			case app.TriggerQuote:
				quote := app.NewID()
				generationSQL(t, store, `INSERT INTO posts(id,author_id,body,quoted_post_id,created_at) VALUES($1,$2,'quote',$3,$4)`, quote, actor, post, at)
				changes["source_post_id"] = quote
			case app.TriggerContinuation:
				changes["source_reply_id"], changes["root_job_id"], changes["chain_depth"] = reply, root.ID, 1
			}
			changes["created_at"], changes["available_at"], changes["expires_at"] = at, at, at.Add(time.Hour)
			job := cloneClaimJob(t, store, root.ID, changes)
			p := eligibilityPolicy()
			if _, err := store.InitializeAgentSettings(context.Background(), app.AgentSettings{AgentID: job.AgentID, PersonaVersion: 1, Enabled: true, Policy: p, UpdatedAt: at}); err != nil {
				t.Fatal(err)
			}
			p.SourceMaxAgeSeconds = 60
			assertExecutionAllowed(t, store, job, p, nil, at.Add(59*time.Second), true)
			assertExecutionAllowed(t, store, job, p, nil, at.Add(time.Minute), false)
			p.SourceMaxAgeSeconds = 604800
			assertExecutionAllowed(t, store, job, p, nil, job.ExpiresAt, false)
			switch kind {
			case app.TriggerHumanPost:
				p.HumanPostProbabilityBPS = 0
			case app.TriggerReply:
				p.ReplyProbabilityBPS = 0
			case app.TriggerRepost:
				p.RepostProbabilityBPS = 0
			case app.TriggerQuote:
				p.QuoteProbabilityBPS = 0
			case app.TriggerContinuation:
				p.ContinuationProbabilityBPS = 0
			}
			assertExecutionAllowed(t, store, job, p, nil, at, false)
		})
	}
}

func TestExecutionEligibilityScheduledWindowAndCaps(t *testing.T) {
	store, original, p := eligibilityFixture(t)
	at := original.CreatedAt.Add(24 * time.Hour)
	key, _ := app.ScheduledGenerationKey(at.Add(-time.Minute))
	job := eligibilityHistory(t, store, original, at, map[string]any{"trigger_kind": "scheduled", "trigger_key": key, "trigger_actor_id": nil, "cooldown_key": nil, "source_post_id": nil, "output_kind": "post"})
	assertExecutionAllowed(t, store, job, p, nil, at, true)
	p.SourceMaxAgeSeconds = 60
	assertExecutionAllowed(t, store, job, p, nil, at, false) // Age from slot, NOT enqueue.
	p.SourceMaxAgeSeconds = 604800
	p.ActiveStart = "10:00"
	assertExecutionAllowed(t, store, job, p, nil, at, false) // Original slot precedes tightened window.
	p.ActiveStart = "09:00"
	assertExecutionAllowed(t, store, job, p, nil, at.Add(7*time.Hour), false)
	assertExecutionAllowed(t, store, job, p, nil, at.Add(24*time.Hour), false)
	p.ScheduledMinPerDay, p.ScheduledMaxPerDay = 0, 0
	assertExecutionAllowed(t, store, job, p, nil, at, false)
	p.ScheduledPostCapPerDay = 0
	assertExecutionAllowed(t, store, job, p, nil, at, false)
	p = eligibilityPolicy()
	priorKey, _ := app.ScheduledGenerationKey(at.Add(-2 * time.Minute))
	eligibilityHistory(t, store, job, at.Add(-2*time.Minute), map[string]any{"trigger_key": priorKey, "cooldown_key": nil, "status": "skipped", "reason_code": "retained", "finished_at": at})
	assertExecutionAllowed(t, store, job, p, nil, at, true)
	p.ScheduledMaxPerDay = 1
	assertExecutionAllowed(t, store, job, p, nil, at, false)
	p.ScheduledPostCapPerDay = 1
	assertExecutionAllowed(t, store, job, p, nil, at, false)
	// Social responses have no active-hours restriction.
	p = eligibilityPolicy()
	p.ActiveStart, p.ActiveEnd = "15:00", "17:00"
	assertExecutionAllowed(t, store, original, p, nil, original.CreatedAt, true)
	legacy := eligibilityHistory(t, store, job, at.Add(24*time.Hour), map[string]any{"cooldown_key": nil})
	assertExecutionAllowed(t, store, legacy, eligibilityPolicy(), nil, legacy.CreatedAt, false)
}

func TestExecutionEligibilityEnqueueLocalDayDST(t *testing.T) {
	for _, test := range []struct {
		name, zone, prior, at string
		want                  bool
	}{
		{"UTC rollover same local day", "America/New_York", "2026-11-01T23:59:58Z", "2026-11-02T00:00:00Z", false},
		{"local rollover same UTC day", "America/New_York", "2026-11-02T04:59:58Z", "2026-11-02T05:00:00Z", true},
		{"25 hour day", "America/New_York", "2026-11-01T04:00:00Z", "2026-11-02T04:59:58Z", false},
		{"23 hour day", "America/New_York", "2026-03-08T05:00:00Z", "2026-03-09T03:59:58Z", false},
		{"missing midnight", "America/Sao_Paulo", "2018-11-04T02:59:58Z", "2018-11-04T03:00:00Z", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, original, p := eligibilityFixture(t)
			prior, _ := time.Parse(time.RFC3339, test.prior)
			at, _ := time.Parse(time.RFC3339, test.at)
			generationSQL(t, store, `UPDATE posts SET created_at=$2 WHERE id=$1`, original.SourcePostID, prior)
			eligibilityHistory(t, store, original, prior, map[string]any{"expires_at": at.Add(3 * 24 * time.Hour)})
			job := eligibilityHistory(t, store, original, at, map[string]any{"expires_at": at.Add(3 * 24 * time.Hour)})
			p.Timezone, p.ReplyCapPerDay = test.zone, 1
			// Publication is a day later. The quota still belongs to enqueue day.
			assertExecutionAllowed(t, store, job, p, nil, at.Add(24*time.Hour), test.want)
		})
	}
}

func TestExecutionEligibilityActionIsolationAndTies(t *testing.T) {
	store, direct, p := eligibilityFixture(t)
	other := socialAgent(t, store, "continuing_agent", nil)
	saveEligibilityPolicy(t, store, other.AgentID, p)
	// Same action key and timestamp, but a different scope. Continuations never
	// consume direct capacity and different roots never consume each other's.
	child := eligibilityHistory(t, store, direct, direct.CreatedAt, map[string]any{"agent_id": other.AgentID, "trigger_kind": "continuation", "trigger_key": direct.TriggerKey, "root_job_id": direct.ID, "chain_depth": 1})
	p.MaxAgentsPerTrigger, p.HumanTriggerCapPerWindow = 1, 1
	assertExecutionAllowed(t, store, direct, p, nil, direct.CreatedAt, true)
	p.HumanTriggerCapPerWindow = 0
	assertExecutionAllowed(t, store, child, p, nil, child.CreatedAt, true)
	peer := socialAgent(t, store, "parallel_agent", nil)
	saveEligibilityPolicy(t, store, peer.AgentID, p)
	otherRoot := eligibilityHistory(t, store, direct, direct.CreatedAt.Add(-2*time.Second), nil)
	eligibilityHistory(t, store, direct, child.CreatedAt, map[string]any{"agent_id": peer.AgentID, "trigger_kind": "continuation", "trigger_key": child.TriggerKey, "root_job_id": otherRoot.ID, "chain_depth": 1})
	assertExecutionAllowed(t, store, child, p, nil, child.CreatedAt, true)
	third := socialAgent(t, store, "same_chain_agent", nil)
	saveEligibilityPolicy(t, store, third.AgentID, p)
	sibling := eligibilityHistory(t, store, child, child.CreatedAt, map[string]any{"agent_id": third.AgentID, "trigger_key": child.TriggerKey, "root_job_id": direct.ID})
	assertExecutionAllowed(t, store, child, p, nil, child.CreatedAt, child.ID < sibling.ID)
	assertExecutionAllowed(t, store, sibling, p, nil, sibling.CreatedAt, sibling.ID < child.ID)
}

func TestExecutionEligibilityChainCapacityAndCausalRoot(t *testing.T) {
	store, root, p := eligibilityFixture(t)
	agent := socialAgent(t, store, "chain_member", nil)
	saveEligibilityPolicy(t, store, agent.AgentID, p)
	// Force the child to rank before its causal root at the same instant, while
	// keeping actual database constraints and immutable root limits intact.
	id := app.ID("00000000-0000-4000-8000-000000000001")
	_, err := generationCloneJob(store, root.ID, map[string]any{"id": id, "root_job_id": root.ID, "agent_id": agent.AgentID, "trigger_kind": "continuation", "chain_depth": 1, "trigger_key": "causal-child"})
	if err != nil {
		t.Fatal(err)
	}
	child := claimJob(t, store, id)
	p.MaxChainDepth, p.MaxChainJobs = 0, 1
	assertExecutionAllowed(t, store, child, p, nil, child.CreatedAt, false)
	p.MaxChainDepth, p.MaxChainJobs = 1, 2
	assertExecutionAllowed(t, store, child, p, nil, child.CreatedAt, true)
	// Fill immutable chain capacity with later reservations; an existing valid
	// member stays valid even if the chain has no room for another child.
	for i := 1; i <= 3; i++ {
		eligibilityHistory(t, store, child, child.CreatedAt.Add(time.Duration(i)*2*time.Second), map[string]any{"root_job_id": root.ID})
	}
	assertExecutionAllowed(t, store, child, p, nil, child.CreatedAt.Add(time.Minute), true)
	late := eligibilityHistory(t, store, child, child.CreatedAt.Add(10*time.Second), map[string]any{"root_job_id": root.ID})
	assertExecutionAllowed(t, store, late, p, nil, late.CreatedAt, false)
	p.MaxChainDepth, p.MaxChainJobs = 9, 10 // Root's five-job bound cannot be expanded.
	assertExecutionAllowed(t, store, late, p, nil, late.CreatedAt, false)
}

func TestExecutionEligibilityReducedHumanWindow(t *testing.T) {
	store, job, p := eligibilityFixture(t)
	p.HumanTriggerCapPerWindow, p.HumanTriggerWindowSeconds = 1, 60
	eligibilityHistory(t, store, job, job.CreatedAt.Add(-time.Minute), nil)
	assertExecutionAllowed(t, store, job, p, nil, job.CreatedAt, false)
	p.HumanTriggerWindowSeconds = 59
	assertExecutionAllowed(t, store, job, p, nil, job.CreatedAt, true)
}

func TestExecutionEligibilityCausalRootAtTiedCapacity(t *testing.T) {
	store, root, p := eligibilityFixture(t)
	p.MaxChainDepth, p.MaxChainJobs = 1, 2
	for i, id := range []app.ID{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"} {
		handle := "first_tied_child"
		if i == 1 {
			handle = "second_tied_child"
		}
		agent := socialAgent(t, store, handle, nil)
		saveEligibilityPolicy(t, store, agent.AgentID, p)
		_, err := generationCloneJob(store, root.ID, map[string]any{"id": id, "root_job_id": root.ID, "agent_id": agent.AgentID, "trigger_kind": "continuation", "chain_depth": 1, "trigger_key": "tied-capacity"})
		if err != nil {
			t.Fatal(err)
		}
		job := claimJob(t, store, id)
		assertExecutionAllowed(t, store, job, p, nil, job.CreatedAt, i == 0)
	}
}

func TestExecutionEligibilityTightenedChainDepth(t *testing.T) {
	store, root, p := eligibilityFixture(t)
	child := eligibilityHistory(t, store, root, root.CreatedAt.Add(2*time.Second), map[string]any{"root_job_id": root.ID, "trigger_kind": "continuation", "chain_depth": 2})
	p.MaxChainDepth = 1
	assertExecutionAllowed(t, store, child, p, nil, child.CreatedAt, false)
	p.MaxChainDepth = 2
	assertExecutionAllowed(t, store, child, p, nil, child.CreatedAt, true)
}
