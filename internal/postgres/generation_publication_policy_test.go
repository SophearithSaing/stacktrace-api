package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestGenerationPublicationOwnReservationAndReducedCaps(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		t.Run(fmt.Sprint(earlier), func(t *testing.T) {
			store, job, attempt, output := publicationFixture(t, app.OutputReply, "An already reserved response")
			if earlier {
				at := job.CreatedAt.Add(-2 * time.Second)
				cloneClaimJob(t, store, job.ID, map[string]any{"output_kind": "quote", "created_at": at, "available_at": at,
					"cooldown_key": string(app.NewID()), "status": "cancelled", "finished_at": at, "lease_expires_at": nil, "reason_code": "retained"})
			}
			generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(jsonb_set(policy,'{reply_cap_per_day}','1'),'{reply_cap_per_conversation}','1') WHERE agent_id=$1`, job.AgentID)
			got, err := store.PublishGeneration(context.Background(), job.ID, 1, attempt.ID, output)
			if earlier {
				if !errors.Is(err, app.ErrForbidden) || got.ID != "" {
					t.Fatalf("earlier quote reservation ignored: %+v %v", got, err)
				}
			} else if err != nil || got.Status != app.JobSucceeded {
				t.Fatalf("own reservation counted twice: %+v %v", got, err)
			}
		})
	}
}

func TestGenerationPublicationSharedResponseAdmissionQuotas(t *testing.T) {
	for _, quota := range []string{"daily", "conversation"} {
		t.Run(quota, func(t *testing.T) {
			store := schedulingStore(t)
			agent := socialAgent(t, store, "quota_agent", func(p *app.GenerationPolicy) {
				if quota == "daily" {
					p.ReplyCapPerDay = 1
				} else {
					p.ReplyCapPerConversation = 1
				}
			})
			actor, session := contentTestActor(t, store, "quota_human")
			post, _ := socialPost(t, store, actor, "Quota conversation")
			at := time.Now().UTC().Add(-10 * time.Second)
			id := app.NewID()
			history := app.GenerationJob{ID: id, AgentID: agent.AgentID, PersonaVersion: 1, TriggerKind: app.TriggerHumanPost,
				TriggerKey: string(id), TriggerActorID: &actor, CooldownKey: string(id), SourcePostID: &post, OutputKind: app.OutputQuote,
				RootJobID: id, MaxChainDepth: 2, MaxChainJobs: 5, Status: app.JobPending, CreatedAt: at, AvailableAt: at, ExpiresAt: at.Add(time.Hour)}
			if err := store.CreateGenerationJob(context.Background(), history); err != nil {
				t.Fatal(err)
			}
			creation, _ := app.NewReplyCreation(post, "@quota_agent a new response opportunity")
			if _, err := store.CreateReply(context.Background(), session, "shared-response-quota", creation); err != nil {
				t.Fatal(err)
			}
			if len(socialJobs(t, store)) != 1 {
				t.Fatal("new reply admission ignored quote history")
			}
		})
	}
}

func TestGenerationPublicationConcurrentLastChainSlot(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprint(full), func(t *testing.T) {
			ctx := context.Background()
			store, root, rootAttempt, rootOutput := publicationFixture(t, app.OutputPost, "Root of a bounded conversation")
			published, err := store.PublishGeneration(ctx, root.ID, 1, rootAttempt.ID, rootOutput)
			if err != nil {
				t.Fatal(err)
			}
			generationSQL(t, store, `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`, root.AgentID)
			left := socialAgent(t, store, "slot_left_publisher", nil)
			right := socialAgent(t, store, "slot_right_publisher", nil)
			socialAgent(t, store, "slot_left_child", nil)
			socialAgent(t, store, "slot_right_child", nil)
			var jobs []app.GenerationJob
			var attempts []app.GenerationAttempt
			var outputs []app.GenerationResult
			for i, agent := range []app.ID{left.AgentID, right.AgentID} {
				p := eligibilityPolicy()
				p.DailyTokenBudget, p.ContinuationProbabilityBPS = 500000, 10000
				if full {
					p.MaxChainJobs = 4
				}
				encoded, _ := json.Marshal(p)
				generationSQL(t, store, `UPDATE agent_settings SET policy=$2 WHERE agent_id=$1`, agent, encoded)
				at := time.Now().UTC().Truncate(time.Microsecond)
				id := app.NewID()
				job := app.GenerationJob{ID: id, AgentID: agent, PersonaVersion: 1, TriggerKind: app.TriggerContinuation,
					TriggerKey: string(id), TriggerActorID: &root.AgentID, CooldownKey: string(id), SourcePostID: published.ResultPostID,
					OutputKind: app.OutputQuote, RootJobID: root.ID, ChainDepth: 1, MaxChainDepth: root.MaxChainDepth, MaxChainJobs: root.MaxChainJobs,
					Status: app.JobPending, CreatedAt: at, AvailableAt: at, ExpiresAt: at.Add(time.Hour)}
				if err := store.CreateGenerationJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				generationSQL(t, store, `UPDATE generation_jobs SET status='running',lease_version=1,lease_expires_at=$2 WHERE id=$1`, id, at.Add(time.Minute))
				job = claimJob(t, store, id)
				input, _ := readGenerationContext(t, store, job)
				body := "Left branch @slot_left_child"
				if i == 1 {
					body = "Right branch @slot_right_child"
				}
				attempt, output := publicationSettle(t, store, job, input, body)
				jobs, attempts, outputs = append(jobs, job), append(attempts, attempt), append(outputs, output)
			}
			// Retained fourth member leaves exactly one immutable root slot. It is
			// later than both publishers, so reduced caps cannot revoke their rank.
			at := time.Now().UTC()
			cloneClaimJob(t, store, jobs[0].ID, map[string]any{"root_job_id": root.ID, "created_at": at, "available_at": at,
				"status": "cancelled", "lease_expires_at": nil, "finished_at": at, "reason_code": "retained", "cooldown_key": string(app.NewID())})
			if full {
				generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{max_chain_jobs}','4') WHERE agent_id IN (SELECT id FROM accounts WHERE handle IN ('slot_left_child','slot_right_child'))`)
			}
			var workers sync.WaitGroup
			start := make(chan struct{})
			for i := range jobs {
				workers.Go(func() {
					<-start
					if _, err := store.PublishGeneration(ctx, jobs[i].ID, 1, attempts[i].ID, outputs[i]); err != nil {
						t.Errorf("reserved chain publisher: %v", err)
					}
				})
			}
			close(start)
			workers.Wait()
			members := 0
			for _, job := range socialJobs(t, store) {
				if job.RootJobID == root.ID {
					members++
				}
			}
			want := 5
			if full {
				want = 4
			}
			if members != want {
				t.Fatalf("chain members=%d want=%d", members, want)
			}
			for _, job := range jobs {
				if claimJob(t, store, job.ID).Status != app.JobSucceeded {
					t.Fatal("full chain incorrectly blocked its reserved publisher")
				}
			}
		})
	}
}
