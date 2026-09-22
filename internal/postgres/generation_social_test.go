package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func socialAgent(t *testing.T, store *Store, handle string, change func(*app.GenerationPolicy)) app.AgentSettings {
	t.Helper()
	s := schedulingSettings()
	p := &s.Policy
	p.ReplyProbabilityBPS, p.RepostProbabilityBPS, p.QuoteProbabilityBPS, p.HumanPostProbabilityBPS, p.ContinuationProbabilityBPS = 10000, 10000, 10000, 10000, 10000
	p.ResponseMinDelaySeconds, p.ResponseMaxDelaySeconds, p.MinSpacingSeconds, p.CooldownSeconds = 0, 0, 1, 1
	p.ReplyCapPerDay, p.ReplyCapPerConversation, p.HumanTriggerCapPerWindow, p.MaxAgentsPerTrigger = 100, 100, 100, 10
	p.MaxChainDepth, p.MaxChainJobs = 5, 10
	if change != nil {
		change(p)
	}
	schedulingAdd(t, store, s)
	generationSQL(t, store, `UPDATE accounts SET handle=$2 WHERE id=$1`, s.AgentID, handle)
	return s
}

func socialPost(t *testing.T, store *Store, author app.ID, body string) (app.ID, time.Time) {
	t.Helper()
	id := app.NewID()
	var at time.Time
	if err := store.db.QueryRow(`INSERT INTO posts(id,author_id,body,created_at) VALUES($1,$2,$3,clock_timestamp()) RETURNING created_at`, id, author, body).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return id, at
}

func socialJobs(t *testing.T, store *Store) []app.GenerationJob {
	t.Helper()
	rows, err := store.db.Query(`SELECT id FROM generation_jobs ORDER BY created_at,id`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []app.ID
	for rows.Next() {
		var id app.ID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var jobs []app.GenerationJob
	for _, id := range ids {
		job, err := store.GenerationJobByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, job)
	}
	return jobs
}

func socialAdmit(store *Store, session string, kind app.GenerationTrigger, action app.ID, now *time.Time, draw func(int64) int64) (int, error) {
	count := 0
	err := store.Transaction(context.Background(), func(q *Queries) error {
		actor, err := q.lockHumanActor(context.Background(), session)
		if err != nil {
			return err
		}
		count, err = q.enqueueSocialGenerationAt(context.Background(), kind, action, actor, now, draw)
		return err
	})
	return count, err
}

func TestGenerationSocialPublicWrites(t *testing.T) {
	for _, kind := range []app.GenerationTrigger{app.TriggerHumanPost, app.TriggerReply, app.TriggerRepost, app.TriggerQuote} {
		t.Run(string(kind), func(t *testing.T) {
			store := schedulingStore(t)
			agent := socialAgent(t, store, "social_agent", nil)
			actor, session := contentTestActor(t, store, "social_human")
			parent, _ := socialPost(t, store, agent.AgentID, "source")
			var action, target app.ID
			ctx := context.Background()
			switch kind {
			case app.TriggerHumanPost, app.TriggerQuote:
				var quoted *app.ID
				if kind == app.TriggerQuote {
					quoted = &parent
				}
				creation, _ := app.NewPostCreation("hello @SOCIAL_AGENT #go", quoted, nil)
				post, err := store.CreatePost(ctx, session, "action", creation)
				if err != nil {
					t.Fatal(err)
				}
				action, target = post.ID, post.ID
				retry, err := store.CreatePost(ctx, session, "action", creation)
				if err != nil || retry.ID != action {
					t.Fatal("retry", err)
				}
			case app.TriggerReply:
				creation, _ := app.NewReplyCreation(parent, "a reply")
				reply, err := store.CreateReply(ctx, session, "action", creation)
				if err != nil {
					t.Fatal(err)
				}
				action, target = reply.Reply.ID, parent
				if _, err := store.CreateReply(ctx, session, "action", creation); err != nil {
					t.Fatal(err)
				}
			case app.TriggerRepost:
				if _, err := store.SetRepost(ctx, session, parent, true); err != nil {
					t.Fatal(err)
				}
				if _, err := store.SetRepost(ctx, session, parent, true); err != nil {
					t.Fatal(err)
				}
				if err := store.db.QueryRow(`SELECT id FROM reposts WHERE account_id=$1 AND post_id=$2`, actor, parent).Scan(&action); err != nil {
					t.Fatal(err)
				}
				target = parent
			}
			jobs := socialJobs(t, store)
			if len(jobs) != 1 {
				t.Fatalf("fresh/retry jobs=%d", len(jobs))
			}
			job := jobs[0]
			key, _ := app.SocialGenerationKey(kind, action)
			if job.AgentID != agent.AgentID || job.TriggerKind != kind || job.OutputKind != app.OutputReply || job.SourcePostID == nil || *job.SourcePostID != target || job.TriggerActorID == nil || *job.TriggerActorID != actor || job.TriggerKey != key || job.RootJobID != job.ID {
				t.Fatalf("wrong target/identity: %+v", job)
			}
			if kind == app.TriggerReply && (job.SourceReplyID == nil || *job.SourceReplyID != action) {
				t.Fatal("reply source lost")
			}
			if kind == app.TriggerRepost && (job.SourceRepostID == nil || *job.SourceRepostID != action) {
				t.Fatal("repost source lost")
			}
			// Private bookmarks and reactions are deliberately non-triggering.
			if _, err := store.SetBookmark(ctx, session, parent, true); err != nil {
				t.Fatal(err)
			}
			reaction := app.ReactionUseful
			if _, err := store.SetReaction(ctx, session, parent, &reaction); err != nil {
				t.Fatal(err)
			}
			if len(socialJobs(t, store)) != 1 {
				t.Fatal("private/non-triggering action enqueued")
			}
		})
	}
}

func TestGenerationSocialCandidatePriority(t *testing.T) {
	store := schedulingStore(t)
	parent := socialAgent(t, store, "parent_agent", nil)
	first := socialAgent(t, store, "first_agent", nil)
	second := socialAgent(t, store, "second_agent", nil)
	tagged := socialAgent(t, store, "tagged_agent", nil)
	disabled := socialAgent(t, store, "disabled_agent", nil)
	generationSQL(t, store, `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`, disabled.AgentID)
	actor, _ := contentTestActor(t, store, "candidate_human")
	source := generationSource{actor: actor, priorityAuthor: parent.AgentID, body: "@SECOND_AGENT @first_agent @second_agent @disabled_agent #go"}
	var ids []app.ID
	err := store.Transaction(context.Background(), func(q *Queries) error {
		var err error
		ids, err = q.generationCandidateIDs(context.Background(), source)
		return err
	})
	if err != nil || len(ids) != 4 || ids[0] != parent.AgentID || ids[1] != second.AgentID || ids[2] != first.AgentID || ids[3] != tagged.AgentID {
		t.Fatalf("priority: %v %v", ids, err)
	}
	source.priorityAuthor = ""
	source.body = "email@first_agent @second_agent.dev @first_agent_extra @@tagged_agent"
	err = store.Transaction(context.Background(), func(q *Queries) error {
		var err error
		ids, err = q.generationCandidateIDs(context.Background(), source)
		return err
	})
	if err != nil || len(ids) != 0 {
		t.Fatalf("embedded mentions: %v %v", ids, err)
	}
	source.actor = first.AgentID
	source.priorityAuthor = first.AgentID
	source.body = "@first_agent"
	err = store.Transaction(context.Background(), func(q *Queries) error {
		var err error
		ids, err = q.generationCandidateIDs(context.Background(), source)
		return err
	})
	if err != nil || len(ids) != 0 {
		t.Fatalf("self selected: %v %v", ids, err)
	}
}

func TestGenerationSocialOptionalConfigAndRollback(t *testing.T) {
	store := schedulingStore(t)
	agent := socialAgent(t, store, "social_agent", nil)
	_, session := contentTestActor(t, store, "rollback_human")
	creation, _ := app.NewPostCreation("@social_agent", nil, nil)
	generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{timezone}','"bad-zone"') WHERE agent_id=$1`, agent.AgentID)
	if _, err := store.CreatePost(context.Background(), session, "invalid-config", creation); err != nil {
		t.Fatal("optional config failed content", err)
	}
	if len(socialJobs(t, store)) != 0 {
		t.Fatal("invalid config admitted")
	}
	data, _ := json.Marshal(agent.Policy)
	generationSQL(t, store, `UPDATE agent_settings SET policy=$2 WHERE agent_id=$1`, agent.AgentID, data)
	generationSQL(t, store, `CREATE FUNCTION fail_social_job() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private sentinel'; END $$;
		CREATE TRIGGER fail_social_job BEFORE INSERT ON generation_jobs FOR EACH ROW EXECUTE FUNCTION fail_social_job()`)
	_, err := store.CreatePost(context.Background(), session, "db-failure", creation)
	if !errors.Is(err, app.ErrUnavailable) || strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("unsafe DB failure: %v", err)
	}
	var posts, keys int
	if err := store.db.QueryRow(`SELECT (SELECT count(*) FROM posts),(SELECT count(*) FROM idempotency_keys WHERE key='db-failure')`).Scan(&posts, &keys); err != nil || posts != 1 || keys != 0 {
		t.Fatalf("partial content/keys: %d %d %v", posts, keys, err)
	}
	if len(socialJobs(t, store)) != 0 {
		t.Fatal("partial jobs")
	}
}

func TestGenerationSocialSourceTimingAndProbability(t *testing.T) {
	for _, scenario := range []string{"delay", "expired", "probability"} {
		t.Run(scenario, func(t *testing.T) {
			store := schedulingStore(t)
			agent := socialAgent(t, store, "timing_agent", func(p *app.GenerationPolicy) {
				p.SourceMaxAgeSeconds = 60
				p.ResponseMinDelaySeconds = 10
				p.ResponseMaxDelaySeconds = 10
				p.HumanPostProbabilityBPS = 5000
			})
			actor, session := contentTestActor(t, store, "timing_human")
			id, created := socialPost(t, store, actor, "@timing_agent")
			now := created.Add(2 * time.Second)
			draw := func(int64) int64 { return 0 }
			want := 1
			if scenario == "expired" {
				now = created.Add(time.Minute)
				want = 0
			}
			if scenario == "probability" {
				draw = func(n int64) int64 { return n - 1 }
				want = 0
			}
			count, err := socialAdmit(store, session, app.TriggerHumanPost, id, &now, draw)
			if err != nil || count != want {
				t.Fatalf("timing: %d %v", count, err)
			}
			if want == 1 {
				job := socialJobs(t, store)[0]
				if job.AgentID != agent.AgentID || !job.CreatedAt.Equal(now) || !job.AvailableAt.Equal(created.Add(10*time.Second)) || !job.ExpiresAt.Equal(created.Add(time.Minute)) {
					t.Fatalf("source timing: %+v", job)
				}
			}
		})
	}
}
