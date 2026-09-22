package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestGenerationSocialRetainedQuotas(t *testing.T) {
	for _, limit := range []string{"human", "day", "conversation", "cooldown", "spacing", "published"} {
		t.Run(limit, func(t *testing.T) {
			store := schedulingStore(t)
			agent := socialAgent(t, store, "quota_agent", func(p *app.GenerationPolicy) {
				switch limit {
				case "human":
					p.HumanTriggerCapPerWindow = 1
				case "day":
					p.ReplyCapPerDay = 1
				case "conversation":
					p.ReplyCapPerConversation = 1
				case "cooldown":
					p.CooldownSeconds = 3600
				case "spacing", "published":
					p.MinSpacingSeconds = 10
				}
			})
			actor, session := contentTestActor(t, store, "quota_human")
			post, _ := socialPost(t, store, agent.AgentID, "source")
			creation, _ := app.NewReplyCreation(post, "reply")
			if _, err := store.CreateReply(context.Background(), session, "first", creation); err != nil {
				t.Fatal(err)
			}
			first := socialJobs(t, store)[0]
			generationSQL(t, store, `UPDATE generation_jobs SET status='cancelled',reason_code='cancelled',finished_at=created_at WHERE id=$1`, first.ID)
			now := first.CreatedAt.Add(2 * time.Second)
			if limit == "published" {
				now = first.CreatedAt.Add(20 * time.Second)
				generationSQL(t, store, `UPDATE agent_settings SET last_published_at=$2 WHERE agent_id=$1`, agent.AgentID, now.Add(-time.Second))
			}
			kind, action := app.TriggerHumanPost, app.NewID()
			if limit == "conversation" || limit == "cooldown" {
				kind = app.TriggerReply
				generationSQL(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,'reply',clock_timestamp())`, action, post, actor)
			} else {
				action, _ = socialPost(t, store, actor, "@quota_agent")
			}
			count, err := socialAdmit(store, session, kind, action, &now, schedulingDraw)
			if err != nil || count != 0 || len(socialJobs(t, store)) != 1 {
				t.Fatalf("bypassed %s: %d %v", limit, count, err)
			}
		})
	}
}

func TestGenerationSocialMixedPolicies(t *testing.T) {
	for _, scenario := range []string{"action", "human_window", "zero"} {
		t.Run(scenario, func(t *testing.T) {
			store := schedulingStore(t)
			first := socialAgent(t, store, "first_agent", func(p *app.GenerationPolicy) {
				if scenario == "action" {
					p.MaxAgentsPerTrigger = 1
				}
				if scenario == "human_window" {
					p.HumanTriggerWindowSeconds = 1
				}
				if scenario == "zero" {
					p.HumanTriggerCapPerWindow = 0
				}
			})
			second := socialAgent(t, store, "second_agent", func(p *app.GenerationPolicy) {
				if scenario == "human_window" {
					p.HumanTriggerCapPerWindow = 1
					p.HumanTriggerWindowSeconds = 3600
				}
			})
			actor, session := contentTestActor(t, store, "mixed_human")
			var now *time.Time
			if scenario == "human_window" {
				creation, _ := app.NewPostCreation("@first_agent", nil, nil)
				if _, err := store.CreatePost(context.Background(), session, "first", creation); err != nil {
					t.Fatal(err)
				}
				stamp := socialJobs(t, store)[0].CreatedAt.Add(10 * time.Second)
				now = &stamp
			}
			id, _ := socialPost(t, store, actor, "@first_agent @second_agent")
			count, err := socialAdmit(store, session, app.TriggerHumanPost, id, now, schedulingDraw)
			want := 1
			if scenario == "human_window" {
				want = 0
			}
			if err != nil || count != want {
				t.Fatalf("mixed policies: %d %v", count, err)
			}
			if scenario == "action" && socialJobs(t, store)[0].AgentID != first.AgentID {
				t.Fatal("priority lost")
			}
			if scenario == "zero" && socialJobs(t, store)[0].AgentID != second.AgentID {
				t.Fatal("zero cap bypassed or disabled other candidate")
			}
		})
	}
	for _, limit := range []string{"action", "human", "day", "conversation", "probability"} {
		t.Run("zero_"+limit, func(t *testing.T) {
			store := schedulingStore(t)
			socialAgent(t, store, "zero_agent", func(p *app.GenerationPolicy) {
				switch limit {
				case "action":
					p.MaxAgentsPerTrigger = 0
				case "human":
					p.HumanTriggerCapPerWindow = 0
				case "day":
					p.ReplyCapPerDay = 0
				case "conversation":
					p.ReplyCapPerConversation = 0
				case "probability":
					p.HumanPostProbabilityBPS = 0
				}
			})
			_, session := contentTestActor(t, store, "zero_human")
			creation, _ := app.NewPostCreation("@zero_agent", nil, nil)
			if _, err := store.CreatePost(context.Background(), session, "zero", creation); err != nil {
				t.Fatal(err)
			}
			if len(socialJobs(t, store)) != 0 {
				t.Fatal("zero cap admitted")
			}
		})
	}
}

func TestGenerationSocialBoundedCandidates(t *testing.T) {
	store := schedulingStore(t)
	for i := range generationCandidateLimit + 8 {
		socialAgent(t, store, fmt.Sprintf("candidate_%02d", i), nil)
	}
	actor, session := contentTestActor(t, store, "bounded_human")
	id, _ := socialPost(t, store, actor, "#go")
	var ids []app.ID
	err := store.Transaction(context.Background(), func(q *Queries) error {
		var err error
		ids, err = q.generationCandidateIDs(context.Background(), generationSource{actor: actor, body: "#go"})
		return err
	})
	if err != nil || len(ids) != generationCandidateLimit {
		t.Fatalf("candidate bound: %d %v", len(ids), err)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			t.Fatal("unstable tag ties")
		}
	}
	count, err := socialAdmit(store, session, app.TriggerHumanPost, id, nil, schedulingDraw)
	if err != nil || count != 10 || len(socialJobs(t, store)) != 10 {
		t.Fatalf("hard action ceiling: %d %v", count, err)
	}
}

func TestGenerationSocialSpacingIncludesScheduled(t *testing.T) {
	store := schedulingStore(t)
	agent := socialAgent(t, store, "spacing_agent", nil)
	_, session := contentTestActor(t, store, "spacing_human")
	creation, _ := app.NewPostCreation("@spacing_agent", nil, nil)
	if _, err := store.CreatePost(context.Background(), session, "reply-reservation", creation); err != nil {
		t.Fatal(err)
	}
	job := socialJobs(t, store)[0]
	// Use a local zone placing the real reservation inside the active window.
	location := "UTC"
	if job.CreatedAt.UTC().Hour() < 9 || job.CreatedAt.UTC().Hour() >= 17 {
		location = "Etc/GMT-12"
	}
	// Full same-day window avoids dependence on wall-clock test execution time.
	generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(jsonb_set(jsonb_set(policy,'{active_start}','"00:00"'),'{active_end}','"23:59"'),'{timezone}',to_jsonb($2::text)) WHERE agent_id=$1`, agent.AgentID, location)
	now := job.CreatedAt
	var result GenerationScheduleResult
	err := store.Transaction(context.Background(), func(q *Queries) error {
		var err error
		result, err = q.scheduleGenerationAgent(context.Background(), agent.AgentID, &now, schedulingDraw)
		return err
	})
	if err != nil || result.JobsEnqueued != 0 || result.SlotsDenied != 1 {
		t.Fatalf("scheduler bypassed reply spacing: %+v %v", result, err)
	}
}
