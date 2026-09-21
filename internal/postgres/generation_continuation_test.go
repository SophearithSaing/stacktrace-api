package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func continuationPending(t *testing.T, store *Store, agent app.ID, depth, jobs int) app.GenerationJob {
	t.Helper()
	var now time.Time
	if err := store.db.QueryRow(`SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	id := app.NewID()
	job := app.GenerationJob{ID: id, AgentID: agent, PersonaVersion: 1, TriggerKind: app.TriggerScheduled, TriggerKey: string(id), OutputKind: app.OutputPost,
		RootJobID: id, MaxChainDepth: depth, MaxChainJobs: jobs, Status: app.JobPending, CreatedAt: now, AvailableAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := store.CreateGenerationJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

// Trusted fixture publication only: no provider, claim, or production publisher.
func continuationPublish(q *Queries, job app.GenerationJob, body string) error {
	ctx := context.Background()
	result, attempt := app.NewID(), app.NewID()
	var err error
	if job.OutputKind == app.OutputReply {
		_, err = q.queryer.ExecContext(ctx, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,$4,$5)`, result, job.SourcePostID, job.AgentID, body, job.AvailableAt)
	} else {
		var quoted *app.ID
		if job.OutputKind == app.OutputQuote {
			quoted = job.SourcePostID
		}
		_, err = q.queryer.ExecContext(ctx, `INSERT INTO posts(id,author_id,body,quoted_post_id,created_at) VALUES($1,$2,$3,$4,$5)`, result, job.AgentID, body, quoted, job.AvailableAt)
	}
	if err != nil {
		return err
	}
	_, err = q.queryer.ExecContext(ctx, `INSERT INTO generation_attempts(id,job_id,attempt_number,lease_version,provider,model,context_hash,context_builder_version,budget_day,reserved_tokens,status,started_at,finished_at)
		VALUES($1,$2,1,1,'fixture','fixture',repeat('a',64),'v1',($3::timestamptz AT TIME ZONE 'UTC')::date,1,'succeeded',$3,$3)`, attempt, job.ID, job.AvailableAt)
	if err != nil {
		return err
	}
	if job.OutputKind == app.OutputReply {
		_, err = q.queryer.ExecContext(ctx, `UPDATE generation_jobs SET status='succeeded',lease_version=1,published_attempt_id=$2,result_reply_id=$3,finished_at=available_at WHERE id=$1`, job.ID, attempt, result)
	} else {
		_, err = q.queryer.ExecContext(ctx, `UPDATE generation_jobs SET status='succeeded',lease_version=1,published_attempt_id=$2,result_post_id=$3,finished_at=available_at WHERE id=$1`, job.ID, attempt, result)
	}
	return err
}

func continuationRun(store *Store, parent app.ID) (int, error) {
	count := 0
	err := store.Transaction(context.Background(), func(q *Queries) error {
		var err error
		count, err = q.EnqueueGenerationContinuations(context.Background(), parent)
		return err
	})
	return count, err
}

func TestGenerationContinuationProvenanceAndRollback(t *testing.T) {
	store := schedulingStore(t)
	actor := socialAgent(t, store, "parent_agent", nil)
	child := socialAgent(t, store, "child_agent", func(p *app.GenerationPolicy) { p.HumanTriggerCapPerWindow = 0 }) // Not a new human allowance.
	parent := continuationPending(t, store, actor.AgentID, 2, 3)
	if _, err := store.Queries.EnqueueGenerationContinuations(context.Background(), parent.ID); !errors.Is(err, errGenerationTransaction) {
		t.Fatal("pool call accepted", err)
	}
	if _, err := continuationRun(store, parent.ID); !errors.Is(err, app.ErrForbidden) {
		t.Fatal("pending parent accepted", err)
	}
	abort := errors.New("abort publication and continuation")
	// Demonstrate same-transaction publication/admission rollback. Acquire the
	// earlier-order account/settings/root locks before changing the parent job.
	err := store.Transaction(context.Background(), func(q *Queries) error {
		if _, err := q.lockGenerationCandidates(context.Background(), generationSource{actor: actor.AgentID, body: "@child_agent"}, true); err != nil {
			return err
		}
		if _, err := q.queryer.ExecContext(context.Background(), `SELECT id FROM generation_jobs WHERE id=$1 FOR UPDATE`, parent.ID); err != nil {
			return err
		}
		if err := continuationPublish(q, parent, "@child_agent"); err != nil {
			return err
		}
		count, err := q.EnqueueGenerationContinuations(context.Background(), parent.ID)
		if err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("continuation count=%d", count)
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	jobs := socialJobs(t, store)
	if len(jobs) != 1 || jobs[0].Status != app.JobPending {
		t.Fatal("publication/child escaped rollback")
	}
	if err := store.Transaction(context.Background(), func(q *Queries) error { return continuationPublish(q, parent, "@child_agent") }); err != nil {
		t.Fatal(err)
	}
	count, err := continuationRun(store, parent.ID)
	if err != nil || count != 1 {
		t.Fatalf("trusted continuation: %d %v", count, err)
	}
	jobs = socialJobs(t, store)
	if len(jobs) != 2 {
		t.Fatal("wrong chain count")
	}
	for _, job := range jobs {
		if job.ID != parent.ID {
			if job.AgentID != child.AgentID || job.RootJobID != parent.ID || job.ChainDepth != 1 || job.MaxChainDepth != 2 || job.MaxChainJobs != 3 || job.TriggerKind != app.TriggerContinuation || *job.TriggerActorID != actor.AgentID || job.OutputKind != app.OutputReply {
				t.Fatalf("wrong continuation: %+v", job)
			}
		}
	}
	if count, err := continuationRun(store, parent.ID); err != nil || count != 0 {
		t.Fatalf("duplicate continuation: %d %v", count, err)
	}
}

func TestGenerationContinuationImmutableAndChildLimits(t *testing.T) {
	for _, scenario := range []string{"root_depth", "child_depth", "child_jobs", "retained_jobs", "action_retry"} {
		t.Run(scenario, func(t *testing.T) {
			store := schedulingStore(t)
			actor := socialAgent(t, store, "parent_agent", nil)
			first := socialAgent(t, store, "first_child", func(p *app.GenerationPolicy) {
				switch scenario {
				case "child_depth":
					p.MaxChainDepth = 0
				case "child_jobs":
					p.MaxChainDepth = 0
					p.MaxChainJobs = 1
				case "action_retry":
					p.MaxAgentsPerTrigger = 1
				}
			})
			second := socialAgent(t, store, "second_child", func(p *app.GenerationPolicy) { p.MaxAgentsPerTrigger = 1 })
			generationSQL(t, store, `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`, second.AgentID)
			depth, jobs := 1, 2
			if scenario == "root_depth" {
				depth = 0
			}
			if scenario == "action_retry" {
				depth, jobs = 2, 4
			}
			parent := continuationPending(t, store, actor.AgentID, depth, jobs)
			if err := store.Transaction(context.Background(), func(q *Queries) error { return continuationPublish(q, parent, "@first_child @second_child") }); err != nil {
				t.Fatal(err)
			}
			generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(jsonb_set(policy,'{max_chain_jobs}','100'),'{max_chain_depth}','10') WHERE agent_id=$1`, actor.AgentID)
			count, err := continuationRun(store, parent.ID)
			want := 0
			if scenario == "retained_jobs" || scenario == "action_retry" {
				want = 1
			}
			if err != nil || count != want {
				t.Fatalf("limit %s: %d %v", scenario, count, err)
			}
			if want == 1 {
				generationSQL(t, store, `UPDATE generation_jobs SET status='cancelled',reason_code='cancelled',finished_at=created_at WHERE root_job_id=$1 AND id<>$1`, parent.ID)
				generationSQL(t, store, `UPDATE agent_settings SET enabled=(agent_id=$2) WHERE agent_id IN ($1,$2)`, first.AgentID, second.AgentID)
				count, err = continuationRun(store, parent.ID)
				if err != nil || count != 0 {
					t.Fatalf("history refunded %s: %d %v", scenario, count, err)
				}
			}
		})
	}
}

func TestGenerationContinuationSiblingRace(t *testing.T) {
	store := schedulingStore(t)
	rootAgent := socialAgent(t, store, "root_agent", nil)
	left := socialAgent(t, store, "left_parent", nil)
	right := socialAgent(t, store, "right_parent", nil)
	socialAgent(t, store, "left_child", nil)
	socialAgent(t, store, "right_child", nil)
	root := continuationPending(t, store, rootAgent.AgentID, 3, 4)
	if err := store.Transaction(context.Background(), func(q *Queries) error { return continuationPublish(q, root, "root") }); err != nil {
		t.Fatal(err)
	}
	root, err := store.GenerationJobByID(context.Background(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	var parents []app.ID
	for i, agent := range []app.ID{left.AgentID, right.AgentID} {
		id := app.NewID()
		parent := app.GenerationJob{ID: id, AgentID: agent, PersonaVersion: 1, TriggerKind: app.TriggerContinuation, TriggerKey: string(id), TriggerActorID: &rootAgent.AgentID, CooldownKey: string(id), SourcePostID: root.ResultPostID,
			OutputKind: app.OutputQuote, RootJobID: root.ID, ChainDepth: 1, MaxChainDepth: 3, MaxChainJobs: 4, Status: app.JobPending, CreatedAt: root.CreatedAt, AvailableAt: root.AvailableAt, ExpiresAt: root.ExpiresAt}
		if err := store.CreateGenerationJob(context.Background(), parent); err != nil {
			t.Fatal(err)
		}
		body := "@left_child"
		if i == 1 {
			body = "@right_child"
		}
		if err := store.Transaction(context.Background(), func(q *Queries) error { return continuationPublish(q, parent, body) }); err != nil {
			t.Fatal(err)
		}
		parents = append(parents, id)
	}
	generationSQL(t, store, `UPDATE agent_settings SET enabled=false WHERE agent_id IN ($1,$2,$3)`, rootAgent.AgentID, left.AgentID, right.AgentID)
	// Distinct quote result posts and target agents avoid accidental source or
	// settings serialization; the common root must serialize the final slot.
	var workers sync.WaitGroup
	counts := make(chan int, 2)
	start := make(chan struct{})
	for _, id := range parents {
		workers.Go(func() {
			<-start
			count, err := continuationRun(store, id)
			if err != nil {
				t.Errorf("sibling: %v", err)
			}
			counts <- count
		})
	}
	close(start)
	workers.Wait()
	close(counts)
	total := 0
	for count := range counts {
		total += count
	}
	if total != 1 || len(socialJobs(t, store)) != 4 {
		t.Fatalf("oversubscribed chain: added=%d jobs=%d", total, len(socialJobs(t, store)))
	}
}

func TestGenerationContinuationRejectsInvalidSource(t *testing.T) {
	for _, scenario := range []string{"disabled_actor", "deleted_post", "spoofed_author", "wrong_attempt"} {
		t.Run(scenario, func(t *testing.T) {
			store := schedulingStore(t)
			actor := socialAgent(t, store, "parent_agent", nil)
			socialAgent(t, store, "child_agent", nil)
			parent := continuationPending(t, store, actor.AgentID, 2, 3)
			if err := store.Transaction(context.Background(), func(q *Queries) error { return continuationPublish(q, parent, "@child_agent") }); err != nil {
				t.Fatal(err)
			}
			parent, err := store.GenerationJobByID(context.Background(), parent.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "disabled_actor":
				generationSQL(t, store, `UPDATE accounts SET disabled_at=clock_timestamp() WHERE id=$1`, actor.AgentID)
			case "deleted_post":
				generationSQL(t, store, `UPDATE posts SET deleted_at=clock_timestamp() WHERE id=$1`, parent.ResultPostID)
			case "spoofed_author":
				human, _ := contentTestActor(t, store, "spoof_human")
				generationSQL(t, store, `UPDATE posts SET author_id=$2 WHERE id=$1`, parent.ResultPostID, human)
			case "wrong_attempt":
				generationSQL(t, store, `ALTER TABLE generation_attempts DISABLE TRIGGER generation_attempts_identity; UPDATE generation_attempts SET status='unknown',error_code='uncertain'; ALTER TABLE generation_attempts ENABLE TRIGGER generation_attempts_identity`)
			}
			count, err := continuationRun(store, parent.ID)
			if count != 0 || scenario != "disabled_actor" && err == nil {
				t.Fatalf("invalid source %s: %d %v", scenario, count, err)
			}
			if len(socialJobs(t, store)) != 1 {
				t.Fatal("invalid source enqueued")
			}
		})
	}
}
