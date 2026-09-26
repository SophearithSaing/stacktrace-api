package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func publicationFixture(t *testing.T, kind app.GenerationOutput, body string) (*Store, app.GenerationJob, app.GenerationAttempt, app.GenerationResult) {
	t.Helper()
	store, job, input := spendFixture(t, 500000)
	if kind != app.OutputReply {
		at := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Microsecond)
		changes := map[string]any{"output_kind": kind, "created_at": at, "available_at": at, "cooldown_key": string(app.NewID())}
		if kind == app.OutputPost {
			key, _ := app.ScheduledGenerationKey(at)
			changes["trigger_kind"], changes["trigger_key"], changes["trigger_actor_id"], changes["cooldown_key"], changes["source_post_id"] = app.TriggerScheduled, key, nil, nil, nil
			p := eligibilityPolicy()
			p.DailyTokenBudget, p.ActiveStart, p.ActiveEnd = 500000, "00:00", "23:59"
			if at.Hour() < 1 || at.Hour() >= 23 {
				p.Timezone = "Etc/GMT+12"
			}
			encoded, _ := json.Marshal(p)
			generationSQL(t, store, `UPDATE agent_settings SET policy=$2 WHERE agent_id=$1`, job.AgentID, encoded)
		}
		job = contextJob(t, store, job, changes)
		input, _ = readGenerationContext(t, store, job)
	}
	attempt, output := publicationSettle(t, store, job, input, body)
	return store, job, attempt, output
}

func publicationSettle(t *testing.T, store *Store, job app.GenerationJob, input app.GenerationContext, body string) (app.GenerationAttempt, app.GenerationResult) {
	t.Helper()
	attempt := admitSpend(t, store, job, input)
	encoded, _ := json.Marshal(map[string]any{"decision": "publish", "body": body})
	if job.OutputKind != app.OutputReply {
		encoded, _ = json.Marshal(map[string]any{"decision": "publish", "body": body, "code": map[string]string{"language": "go", "filename": "main.go", "source": "fmt.Println(42)"}})
	}
	output, err := app.DecodeGenerationResult(encoded, job)
	if err != nil {
		t.Fatal(err)
	}
	inputTokens, outputTokens := int64(100), int64(20)
	if ok, err := store.SettleGeneration(context.Background(), attempt, app.GenerationOutcome{Result: output, InputTokens: &inputTokens, OutputTokens: &outputTokens}, "publication-fixture"); err != nil || !ok {
		t.Fatalf("settlement: %v %v", ok, err)
	}
	return storedSpend(t, store, attempt.ID), output
}

func TestGenerationPublicationIdentityReplayAndProjections(t *testing.T) {
	for _, kind := range []app.GenerationOutput{app.OutputPost, app.OutputReply, app.OutputQuote} {
		t.Run(string(kind), func(t *testing.T) {
			store, job, attempt, output := publicationFixture(t, kind, "A careful #Publication example @publication_child")
			child := socialAgent(t, store, "publication_child", nil)
			ctx := context.Background()
			published, err := store.PublishGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, output)
			if err != nil {
				t.Fatal(err)
			}
			if published.Status != app.JobSucceeded || published.PublishedAttemptID == nil || *published.PublishedAttemptID != attempt.ID || published.LeaseExpiresAt != nil || !sameClaimJob(published, claimJob(t, store, job.ID)) {
				t.Fatalf("publication: %+v", published)
			}
			settings, err := store.AgentSettingsByID(ctx, job.AgentID)
			if err != nil || settings.LastPublishedAt == nil || !settings.LastPublishedAt.Equal(*published.FinishedAt) {
				t.Fatalf("last publication: %+v %v", settings, err)
			}
			children := 0
			for _, candidate := range socialJobs(t, store) {
				if candidate.RootJobID == job.ID && candidate.ID != job.ID {
					children++
					if candidate.AgentID != child.AgentID || candidate.TriggerKind != app.TriggerContinuation || candidate.ChainDepth != 1 || *candidate.TriggerActorID != job.AgentID {
						t.Fatalf("child: %+v", candidate)
					}
				}
			}
			if children != 1 {
				t.Fatalf("children=%d", children)
			}
			if kind == app.OutputReply {
				reply, err := store.replyResultByID(ctx, *published.ResultReplyID)
				if err != nil || !reply.Reply.IsGenerated || reply.Reply.Author.ID != job.AgentID || reply.Reply.PostID != *job.SourcePostID || reply.Reply.Body != output.Content().Body || !reply.Reply.CreatedAt.Equal(*published.FinishedAt) {
					t.Fatalf("reply: %+v %v", reply, err)
				}
				page, err := store.ListReplies(ctx, *job.SourcePostID, app.ReadWindow{})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, item := range page.Items {
					if item.ID == reply.Reply.ID {
						found = item.IsGenerated
					} else if item.IsGenerated {
						t.Fatal("legacy reply marked generated")
					}
				}
				post, err := store.PostByID(ctx, *job.SourcePostID, "")
				if err != nil || post.IsGenerated || !found || len(post.ReplyPreview.Items) != 2 || !post.ReplyPreview.Items[1].IsGenerated {
					t.Fatalf("preview/list: %+v %v found=%v", post, err, found)
				}
			} else {
				post, err := store.PostByID(ctx, *published.ResultPostID, "")
				if err != nil || !post.IsGenerated || post.Author.ID != job.AgentID || post.Content.Body != output.Content().Body || post.Content.Code == nil || len(post.Content.Tags) != 1 || !post.CreatedAt.Equal(*published.FinishedAt) {
					t.Fatalf("post: %+v %v", post, err)
				}
				if kind == app.OutputQuote && (post.Quote == nil || post.Quote.ID != *job.SourcePostID) || kind == app.OutputPost && post.Quote != nil {
					t.Fatalf("quote: %+v", post.Quote)
				}
			}
			before := publicationCounts(t, store)
			// Exact replay is independent of subsequent pause, deletion and expiry.
			generationSQL(t, store, `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`, job.AgentID)
			for range 2 {
				replay, err := store.PublishGeneration(ctx, job.ID, job.LeaseVersion, attempt.ID, output)
				if err != nil || !sameClaimJob(replay, published) || publicationCounts(t, store) != before {
					t.Fatalf("replay: %+v %v", replay, err)
				}
			}
			if _, err := store.PublishGeneration(ctx, job.ID, job.LeaseVersion+1, attempt.ID, output); !errors.Is(err, app.ErrConflict) {
				t.Fatal("stale replay", err)
			}
		})
	}
}

func publicationCounts(t *testing.T, store *Store) string {
	t.Helper()
	var counts string
	if err := store.db.QueryRow(`SELECT concat((SELECT count(*) FROM posts),':',(SELECT count(*) FROM replies),':',
		(SELECT count(*) FROM tags),':',(SELECT count(*) FROM post_tags),':',(SELECT count(*) FROM generation_jobs))`).Scan(&counts); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestGenerationPublicationDenialsRollback(t *testing.T) {
	for _, denial := range []string{"wrong_output", "wrong_attempt", "wrong_lease", "skip", "pause", "source", "policy", "repetition", "unsafe"} {
		t.Run(denial, func(t *testing.T) {
			body := "A new #Rollback publication"
			if denial == "unsafe" {
				body = "I have live access to secrets #Denied"
			}
			store, job, attempt, output := publicationFixture(t, app.OutputQuote, body)
			want := app.ErrGenerationOutput
			version := job.LeaseVersion
			switch denial {
			case "wrong_output":
				output, _ = app.DecodeGenerationResult([]byte(`{"decision":"publish","body":"A different #Denied output"}`), job)
			case "wrong_attempt":
				attempt.ID, want = app.NewID(), app.ErrNotFound
			case "wrong_lease":
				version++
				want = app.ErrConflict
			case "skip":
				output, _ = app.DecodeGenerationResult([]byte(`{"decision":"skip","reason":"not_relevant"}`), job)
			case "pause":
				generationSQL(t, store, `UPDATE agent_settings SET enabled=false WHERE agent_id=$1`, job.AgentID)
				want = app.ErrForbidden
			case "source":
				generationSQL(t, store, `UPDATE posts SET deleted_at=clock_timestamp() WHERE id=$1`, job.SourcePostID)
				want = app.ErrDeleted
			case "policy":
				generationSQL(t, store, `UPDATE agent_settings SET policy=jsonb_set(policy,'{reply_cap_per_day}','0') WHERE agent_id=$1`, job.AgentID)
				want = app.ErrForbidden
			case "repetition":
				socialPost(t, store, job.AgentID, output.Content().Body)
				want = app.ErrGenerationRepetition
			case "unsafe":
				want = app.ErrGenerationUnsafe
			}
			before := publicationCounts(t, store)
			got, err := store.PublishGeneration(context.Background(), job.ID, version, attempt.ID, output)
			if !errors.Is(err, want) || got.ID != "" || publicationCounts(t, store) != before || !sameClaimJob(job, claimJob(t, store, job.ID)) {
				t.Fatalf("denial: %+v %v want=%v", got, err, want)
			}
		})
	}
}

func TestGenerationPublicationLegacySuccessIsNotAuthority(t *testing.T) {
	for _, digest := range []bool{false, true} {
		t.Run(fmt.Sprint(digest), func(t *testing.T) {
			store, job, input := spendFixture(t, 500000)
			attempt := admitSpend(t, store, job, input)
			output, _ := app.DecodeGenerationResult([]byte(`{"decision":"publish","body":"Legacy output"}`), job)
			var saved any
			if digest {
				saved = output.Digest()
			}
			generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=clock_timestamp(),output_digest=$2 WHERE id=$1`, attempt.ID, saved)
			before := publicationCounts(t, store)
			if got, err := store.PublishGeneration(context.Background(), job.ID, 1, attempt.ID, output); !errors.Is(err, app.ErrGenerationOutput) || got.ID != "" || publicationCounts(t, store) != before {
				t.Fatalf("legacy authority: %+v %v", got, err)
			}
		})
	}
}

func TestGenerationPublicationWriteFailures(t *testing.T) {
	for _, table := range []string{"posts", "replies", "post_tags", "generation_jobs", "agent_settings"} {
		for _, failure := range []string{"zero_rows", "after_write", "commit"} {
			t.Run(fmt.Sprintf("%s/%s", table, failure), func(t *testing.T) {
				kind := app.OutputQuote
				if table == "replies" {
					kind = app.OutputReply
				}
				store, job, attempt, output := publicationFixture(t, kind, "A transaction #Rollback example @rollback_child")
				socialAgent(t, store, "rollback_child", nil)
				event := "INSERT"
				if table == "generation_jobs" || table == "agent_settings" {
					event = "UPDATE"
				}
				timing, body := "BEFORE", "RETURN NULL;"
				if failure != "zero_rows" {
					timing, body = "AFTER", "RAISE EXCEPTION 'secret publication failure';"
				}
				generationSQL(t, store, `CREATE FUNCTION reject_publication() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN `+body+` END $$`)
				prefix, deferred := "", ""
				if failure == "commit" {
					prefix, deferred = "CONSTRAINT ", " DEFERRABLE INITIALLY DEFERRED"
				}
				generationSQL(t, store, `CREATE `+prefix+`TRIGGER reject_publication `+timing+` `+event+` ON `+table+deferred+` FOR EACH ROW EXECUTE FUNCTION reject_publication()`)
				before := publicationCounts(t, store)
				got, err := store.PublishGeneration(context.Background(), job.ID, 1, attempt.ID, output)
				if !errors.Is(err, app.ErrUnavailable) || got.ID != "" || publicationCounts(t, store) != before || !sameClaimJob(job, claimJob(t, store, job.ID)) {
					t.Fatalf("failure: %+v %v", got, err)
				}
				settings, err := store.AgentSettingsByID(context.Background(), job.AgentID)
				if err != nil || settings.LastPublishedAt != nil {
					t.Fatal("last publication escaped rollback", err)
				}
			})
		}
	}
}
