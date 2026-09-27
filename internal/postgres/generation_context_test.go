package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func contextJob(t *testing.T, store *Store, root app.GenerationJob, changes map[string]any) app.GenerationJob {
	t.Helper()
	var now time.Time
	if err := store.db.QueryRow(`SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"created_at": now.Add(-time.Minute), "available_at": now.Add(-time.Minute),
		"expires_at": now.Add(time.Hour), "status": "running", "lease_version": 1, "lease_expires_at": now.Add(time.Minute)}
	for key, value := range changes {
		values[key] = value
	}
	return cloneClaimJob(t, store, root.ID, values)
}

func readGenerationContext(t *testing.T, store *Store, job app.GenerationJob) (app.GenerationContext, app.PublicGenerationContext) {
	t.Helper()
	built, err := store.GenerationContext(context.Background(), job.ID, job.LeaseVersion)
	if err != nil {
		t.Fatal(err)
	}
	var public app.PublicGenerationContext
	if err := json.Unmarshal([]byte(built.PublicJSON()), &public); err != nil {
		t.Fatal(err)
	}
	if !built.Matches(job) || len(built.PublicJSON())+len(built.Instructions()) > app.MaxGenerationContextBytes {
		t.Fatal("invalid binding/bounds")
	}
	return built, public
}

func contextSocialChanges(actor, post app.ID, trigger app.GenerationTrigger) map[string]any {
	return map[string]any{"trigger_kind": trigger, "trigger_actor_id": actor, "source_post_id": post, "cooldown_key": "private-cooldown", "output_kind": "reply"}
}

func TestGenerationContextPinnedPersonaLeaseAndPrivacy(t *testing.T) {
	store, persona, root := generationSetup(t)
	ctx := context.Background()
	job := contextJob(t, store, root, nil)
	persona.Version, persona.Instructions = 2, "New selected instructions"
	if err := store.CreatePersona(ctx, persona); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InitializeAgentSettings(ctx, app.AgentSettings{AgentID: job.AgentID, PersonaVersion: 2, Policy: generationPolicy(), UpdatedAt: job.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	built, public := readGenerationContext(t, store, job)
	if built.Instructions() != "Discuss practical Go." || public.Source != nil || public.TriggerReply != nil || public.RepostActor != nil || !strings.Contains(built.PublicJSON(), `"interaction":"none"`) {
		t.Fatalf("original context: %s", built.PublicJSON())
	}
	for _, private := range []string{string(job.ID), string(job.AgentID), job.TriggerKey, "lease", "persona_version", "job", "settings", "session", "bookmark", "password", "created_at", "display_name"} {
		if strings.Contains(built.PublicJSON(), private) {
			t.Fatalf("private field leaked: %s", private)
		}
	}
	for _, version := range []int64{0, 2} {
		if got, err := store.GenerationContext(ctx, job.ID, version); !errors.Is(err, app.ErrConflict) || got.PublicJSON() != "" {
			t.Fatalf("stale lease: %+v %v", got, err)
		}
	}
	for name, changes := range map[string]map[string]any{
		"expired lease": {"lease_expires_at": job.AvailableAt.Add(time.Second)},
		"expired job":   {"expires_at": job.AvailableAt.Add(time.Second)},
		"pending":       {"status": "pending", "lease_version": 0, "lease_expires_at": nil},
		"future":        {"available_at": job.ExpiresAt.Add(-time.Minute), "lease_expires_at": job.ExpiresAt},
	} {
		t.Run(name, func(t *testing.T) {
			other := contextJob(t, store, root, changes)
			if _, err := store.GenerationContext(ctx, other.ID, other.LeaseVersion); !errors.Is(err, app.ErrConflict) {
				t.Fatal(err)
			}
		})
	}
	// Exact equality, including the final check, is tested with a database clock
	// projection, not a timing-dependent sleep at a microsecond boundary.
	for _, expiry := range []time.Time{*job.LeaseExpiresAt, job.ExpiresAt} {
		err := store.readSnapshot(ctx, func(q *Queries) error {
			q.queryer = contextClockQueryer{queryer: q.queryer, at: expiry}
			_, err := q.generationContext(ctx, job.ID, 1)
			return err
		})
		if !errors.Is(err, app.ErrConflict) {
			t.Fatalf("exclusive expiry: %v", err)
		}
	}
}

type contextClockQueryer struct {
	queryer
	at time.Time
}

func (q contextClockQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "clock_timestamp()") {
		return q.queryer.QueryRowContext(ctx, `SELECT $1::timestamptz`, q.at)
	}
	return q.queryer.QueryRowContext(ctx, query, args...)
}

func TestGenerationContextTriggerWindowAndKinds(t *testing.T) {
	store, _, root := generationSetup(t)
	actor, post, reply, repost := generationSocial(t, store, root)
	changes := contextSocialChanges(actor, post, app.TriggerReply)
	changes["source_reply_id"] = reply
	job := contextJob(t, store, root, changes)
	for i := 1; i <= 15; i++ {
		id := app.ID(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
		generationSQL(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,$4,$5)`, id, post, actor, fmt.Sprintf("reply %02d", i), job.CreatedAt)
	}
	disabled, _ := contentTestActor(t, store, "hidden_contributor")
	generationSQL(t, store, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, disabled)
	generationSQL(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,'hidden contribution',now())`, app.NewID(), post, disabled)
	generationSQL(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at,deleted_at) VALUES($1,$2,$3,'deleted contribution',now(),now())`, app.NewID(), post, actor)
	generationSQL(t, store, `UPDATE accounts SET bio='profile sentinel',status_text='status sentinel' WHERE id=$1`, actor)
	built, public := readGenerationContext(t, store, job)
	if public.Source.Content.Body != "source post" || public.TriggerReply.Content.Body != "source reply" || len(public.Replies) != 10 || !strings.Contains(built.PublicJSON(), `"interaction":"reply"`) {
		t.Fatalf("context: %s", built.PublicJSON())
	}
	for i, item := range public.Replies {
		if item.Content.Body != fmt.Sprintf("reply %02d", i+6) || item.Author.Handle != "generation_actor" || item.Author.Type != app.AccountHuman || item.Kind != app.OutputReply {
			t.Fatalf("history order/attribution: %+v", public.Replies)
		}
	}
	for _, private := range []string{string(job.ID), string(post), string(reply), string(actor), job.CooldownKey, "profile sentinel", "status sentinel", "hidden contribution", "deleted contribution"} {
		if strings.Contains(built.PublicJSON(), private) {
			t.Fatalf("non-public context: %s", private)
		}
	}
	// Continuations derive the public interaction from their source shape.
	for _, withReply := range []bool{false, true} {
		changes := contextSocialChanges(actor, post, app.TriggerContinuation)
		changes["root_job_id"], changes["chain_depth"] = root.ID, 1
		interaction := "post"
		if withReply {
			changes["source_reply_id"] = reply
			interaction = "reply"
		}
		built, _ := readGenerationContext(t, store, contextJob(t, store, root, changes))
		if !strings.Contains(built.PublicJSON(), `"interaction":"`+interaction+`"`) {
			t.Fatal(built.PublicJSON())
		}
	}
	quote := app.NewID()
	generationSQL(t, store, `INSERT INTO posts(id,author_id,body,quoted_post_id,created_at) VALUES($1,$2,'quote body',$3,now())`, quote, actor, post)
	built, public = readGenerationContext(t, store, contextJob(t, store, root, contextSocialChanges(actor, quote, app.TriggerQuote)))
	if public.Source.Kind != app.OutputQuote || !strings.Contains(built.PublicJSON(), `"interaction":"quote"`) {
		t.Fatal(built.PublicJSON())
	}
	// A quote is itself canonical; deletion of its quoted original does not hide it.
	generationSQL(t, store, `UPDATE posts SET deleted_at=now() WHERE id=$1`, post)
	readGenerationContext(t, store, contextJob(t, store, root, contextSocialChanges(actor, quote, app.TriggerQuote)))
	generationSQL(t, store, `UPDATE posts SET deleted_at=NULL WHERE id=$1`, post)
	other, _ := contentTestActor(t, store, "repost_actor")
	generationSQL(t, store, `UPDATE reposts SET account_id=$2 WHERE id=$1`, repost, other)
	changes = contextSocialChanges(other, post, app.TriggerRepost)
	changes["source_repost_id"] = repost
	built, public = readGenerationContext(t, store, contextJob(t, store, root, changes))
	if public.RepostActor.Handle != "repost_actor" || public.Source.Author.Handle != "generation_actor" || !strings.Contains(built.PublicJSON(), `"interaction":"repost"`) {
		t.Fatal(built.PublicJSON())
	}
}

func TestGenerationContextRecentAgentContent(t *testing.T) {
	store, _, root := generationSetup(t)
	actor, post, _, _ := generationSocial(t, store, root)
	job := contextJob(t, store, root, nil)
	generationSQL(t, store, `INSERT INTO reposts(id,post_id,account_id,created_at) VALUES($1,$2,$3,now())`, app.NewID(), post, job.AgentID)
	for i := 1; i <= 24; i++ {
		id := app.ID(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
		body := fmt.Sprintf("own %02d", i)
		if i%3 == 0 {
			generationSQL(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,$4,$5)`, id, post, job.AgentID, body, job.CreatedAt)
		} else {
			var quoted *app.ID
			if i%3 == 2 {
				quoted = &post
			}
			generationSQL(t, store, `INSERT INTO posts(id,author_id,body,created_at,quoted_post_id) VALUES($1,$2,$3,$4,$5)`, id, job.AgentID, body, job.CreatedAt, quoted)
		}
	}
	// Newer invisible content must not displace the valid window.
	deletedParent := app.NewID()
	generationSQL(t, store, `INSERT INTO posts(id,author_id,body,deleted_at,created_at) VALUES($1,$2,'deleted parent',now(),now())`, deletedParent, actor)
	generationSQL(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,'hidden reply',now())`, app.NewID(), deletedParent, job.AgentID)
	generationSQL(t, store, `INSERT INTO posts(id,author_id,body,deleted_at,created_at) VALUES($1,$2,'hidden post',now(),now())`, app.NewID(), job.AgentID)
	generationSQL(t, store, `INSERT INTO replies(id,post_id,author_id,body,deleted_at,created_at) VALUES($1,$2,$3,'deleted reply',now(),now())`, app.NewID(), post, job.AgentID)
	disabled, _ := contentTestActor(t, store, "disabled_parent")
	parent := app.NewID()
	generationSQL(t, store, `INSERT INTO posts(id,author_id,body,created_at) VALUES($1,$2,'disabled parent',now())`, parent, disabled)
	generationSQL(t, store, `UPDATE accounts SET disabled_at=now() WHERE id=$1`, disabled)
	generationSQL(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,'disabled parent reply',now())`, app.NewID(), parent, job.AgentID)
	built, public := readGenerationContext(t, store, job)
	if len(public.RecentAgentContent) != 20 {
		t.Fatal(built.PublicJSON())
	}
	for i, item := range public.RecentAgentContent {
		wantKind := []app.GenerationOutput{app.OutputReply, app.OutputPost, app.OutputQuote}[(i+5)%3]
		if item.Content.Body != fmt.Sprintf("own %02d", i+5) || item.Author.Handle != "generation_agent" || item.Author.Type != app.AccountAgent || item.Kind != wantKind {
			t.Fatalf("recent item %d: %+v", i, item)
		}
	}
	// Reposts are not authored content; foreign public bodies are not agent history.
	if strings.Contains(built.PublicJSON(), "source post") || strings.Contains(built.PublicJSON(), "hidden") || strings.Contains(built.PublicJSON(), "disabled") {
		t.Fatal(built.PublicJSON())
	}
}

func TestGenerationContextByteBudget(t *testing.T) {
	store, persona, root := generationSetup(t)
	persona.Version, persona.Instructions = 2, strings.Repeat("x", 16000)
	if err := store.CreatePersona(context.Background(), persona); err != nil {
		t.Fatal(err)
	}
	actor, post, reply, _ := generationSocial(t, store, root)
	changes := contextSocialChanges(actor, post, app.TriggerReply)
	changes["source_reply_id"] = reply
	changes["persona_version"] = 2
	job := contextJob(t, store, root, changes)
	for i := 1; i <= 20; i++ {
		generationSQL(t, store, `INSERT INTO posts(id,author_id,body,code_language,code_filename,code_source,created_at) VALUES($1,$2,$3,'go','main.go',$4,$5)`, app.NewID(), job.AgentID, fmt.Sprintf("history %02d", i), strings.Repeat("x", 1500), job.CreatedAt.Add(time.Duration(i)*time.Second))
	}
	built, public := readGenerationContext(t, store, job)
	if built.Instructions() != persona.Instructions || len(public.RecentAgentContent) == 0 || len(public.RecentAgentContent) >= 20 || public.Source.Content.Body != "source post" || public.TriggerReply.Content.Body != "source reply" {
		t.Fatal(built.PublicJSON())
	}
	for i, item := range public.RecentAgentContent {
		if item.Content.Body != fmt.Sprintf("history %02d", 21-len(public.RecentAgentContent)+i) {
			t.Fatal("did not drop oldest")
		}
	}
	again, _ := readGenerationContext(t, store, job)
	if again.PublicJSON() != built.PublicJSON() {
		t.Fatal("nondeterministic byte selection")
	}
	// Valid per-item code can exceed the serialized budget through JSON escaping.
	// Required source is never silently truncated or removed to make it fit.
	generationSQL(t, store, `UPDATE posts SET code_language='go',code_filename='main.go',code_source=$2 WHERE id=$1`, post, strings.Repeat("<", 5000))
	if got, err := store.GenerationContext(context.Background(), job.ID, 1); !errors.Is(err, app.ErrGenerationOutput) || got.PublicJSON() != "" {
		t.Fatalf("oversized required source: %v", err)
	}
}

// The wrapper performs concurrent committed writes only after the snapshot's
// first job read. It also counts round trips to guard against per-item hydration.
type contextObserveQueryer struct {
	queryer
	observe func(string)
	calls   int
}

func (q *contextObserveQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.calls++
	q.observe(query)
	return q.queryer.QueryRowContext(ctx, query, args...)
}
func (q *contextObserveQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.calls++
	q.observe(query)
	return q.queryer.QueryContext(ctx, query, args...)
}

func TestGenerationContextSnapshotAndCancellation(t *testing.T) {
	store, _, root := generationSetup(t)
	actor, post, reply, _ := generationSocial(t, store, root)
	changes := contextSocialChanges(actor, post, app.TriggerReply)
	changes["source_reply_id"] = reply
	job := contextJob(t, store, root, changes)
	// A full history still needs only one hydration query, not 30 item queries.
	for i := 0; i < 20; i++ {
		generationSQL(t, store, `INSERT INTO posts(id,author_id,body,created_at) VALUES($1,$2,'own history',now())`, app.NewID(), job.AgentID)
		generationSQL(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,'conversation history',now())`, app.NewID(), post, actor)
	}
	ctx := context.Background()
	var built app.GenerationContext
	err := store.readSnapshot(ctx, func(q *Queries) error {
		changed := false
		observed := &contextObserveQueryer{queryer: q.queryer, observe: func(query string) {
			if !changed && strings.Contains(query, "clock_timestamp()") {
				changed = true
				generationSQL(t, store, `UPDATE posts SET body='new source',deleted_at=now() WHERE id=$1`, post)
				generationSQL(t, store, `UPDATE replies SET body='new trigger' WHERE id=$1`, reply)
			}
		}}
		q.queryer = observed
		var err error
		built, err = q.generationContext(ctx, job.ID, 1)
		if observed.calls != 7 {
			t.Errorf("context queries=%d, want 7", observed.calls)
		}
		return err
	})
	if err != nil || !strings.Contains(built.PublicJSON(), "source post") || !strings.Contains(built.PublicJSON(), "source reply") || strings.Contains(built.PublicJSON(), "new source") {
		t.Fatalf("mixed snapshot: %s %v", built.PublicJSON(), err)
	}
	if _, err := store.GenerationContext(ctx, job.ID, 1); !errors.Is(err, app.ErrDeleted) {
		t.Fatalf("new snapshot missed deletion: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := store.GenerationContext(cancelled, job.ID, 1); !errors.Is(err, context.Canceled) || got.PublicJSON() != "" {
		t.Fatalf("cancellation: %v", err)
	}
	if !sameClaimJob(job, claimJob(t, store, job.ID)) {
		t.Fatal("read mutated job")
	}
}

func TestGenerationContextRejectCorruptPersonaAndAuthor(t *testing.T) {
	for _, field := range []string{"instructions", "tags bytes", "tags shape", "handle", "type", "trigger body", "optional body"} {
		t.Run(field, func(t *testing.T) {
			store, _, root := generationSetup(t)
			actor, post, reply, _ := generationSocial(t, store, root)
			changes := contextSocialChanges(actor, post, app.TriggerReply)
			changes["source_reply_id"] = reply
			job := contextJob(t, store, root, changes)
			switch field {
			case "instructions", "tags bytes", "tags shape":
				// Only this isolated corruption fixture bypasses persona immutability.
				generationSQL(t, store, `ALTER TABLE agent_personas DISABLE TRIGGER USER`)
				if field == "instructions" {
					generationSQL(t, store, `ALTER TABLE agent_personas DROP CONSTRAINT agent_personas_instructions_check`)
					generationSQL(t, store, `UPDATE agent_personas SET instructions=repeat('x',100000)`)
				} else if field == "tags bytes" {
					generationSQL(t, store, `UPDATE agent_personas SET topic_tags=ARRAY[repeat('x',100000)]`)
				} else {
					generationSQL(t, store, `UPDATE agent_personas SET topic_tags=ARRAY['UPPERCASE']`)
				}
			case "handle":
				generationSQL(t, store, `ALTER TABLE accounts DROP CONSTRAINT accounts_handle_check`)
				generationSQL(t, store, `UPDATE accounts SET handle=repeat('x',100000) WHERE id=$1`, actor)
			case "type":
				generationSQL(t, store, `ALTER TABLE accounts DROP CONSTRAINT accounts_type_check`)
				generationSQL(t, store, `UPDATE accounts SET type=repeat('x',100000) WHERE id=$1`, actor)
			case "trigger body":
				generationSQL(t, store, `ALTER TABLE replies DROP CONSTRAINT replies_body_check`)
				generationSQL(t, store, `UPDATE replies SET body=repeat('x',100000) WHERE id=$1`, reply)
			case "optional body":
				generationSQL(t, store, `ALTER TABLE posts DROP CONSTRAINT posts_body_check`)
				generationSQL(t, store, `INSERT INTO posts(id,author_id,body,created_at) VALUES($1,$2,repeat('x',100000),now())`, app.NewID(), job.AgentID)
			}
			if got, err := store.GenerationContext(context.Background(), job.ID, 1); !errors.Is(err, app.ErrGenerationOutput) || got.PublicJSON() != "" {
				t.Fatalf("corruption: %v", err)
			}
		})
	}
}

type contextFinalClockQueryer struct {
	queryer
	job    app.GenerationJob
	clocks int
}

func (q *contextFinalClockQueryer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "clock_timestamp()") {
		q.clocks++
		now := q.job.AvailableAt
		if q.clocks == 2 {
			now = *q.job.LeaseExpiresAt
		}
		return q.queryer.QueryRowContext(ctx, `SELECT $1::timestamptz`, now)
	}
	return q.queryer.QueryRowContext(ctx, query, args...)
}

func TestGenerationContextExpiryDuringRead(t *testing.T) {
	store, _, root := generationSetup(t)
	job := contextJob(t, store, root, nil)
	err := store.readSnapshot(context.Background(), func(q *Queries) error {
		clock := &contextFinalClockQueryer{queryer: q.queryer, job: job}
		q.queryer = clock
		built, err := q.generationContext(context.Background(), job.ID, 1)
		if clock.clocks != 2 || built.PublicJSON() != "" {
			t.Fatal("missing final lease check")
		}
		return err
	})
	if !errors.Is(err, app.ErrConflict) {
		t.Fatalf("expired during snapshot: %v", err)
	}
}

func TestGenerationContextRejectCorruptPublicFields(t *testing.T) {
	for _, test := range []struct {
		name, setup, query string
		value              any
	}{
		{"body bytes", `ALTER TABLE posts DROP CONSTRAINT posts_body_check`, `UPDATE posts SET body=$2 WHERE id=$1`, strings.Repeat("x", 100000)},
		{"body runes", `ALTER TABLE posts DROP CONSTRAINT posts_body_check`, `UPDATE posts SET body=$2 WHERE id=$1`, strings.Repeat("x", 321)},
		{"code language", `ALTER TABLE posts DROP CONSTRAINT posts_check`, `UPDATE posts SET code_language=$2,code_filename='a.go',code_source='source' WHERE id=$1`, strings.Repeat("x", 100000)},
		{"code filename", `ALTER TABLE posts DROP CONSTRAINT posts_check`, `UPDATE posts SET code_language='go',code_filename=$2,code_source='source' WHERE id=$1`, strings.Repeat("x", 100000)},
		{"code source", `ALTER TABLE posts DROP CONSTRAINT posts_check`, `UPDATE posts SET code_language='go',code_filename='a.go',code_source=$2 WHERE id=$1`, strings.Repeat("x", 100000)},
		{"partial code", `ALTER TABLE posts DROP CONSTRAINT posts_check`, `UPDATE posts SET code_language=$2 WHERE id=$1`, "go"},
		{"tags", "", `UPDATE posts SET body=$2 WHERE id=$1`, "#a #b #c #d #e #f"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _, root := generationSetup(t)
			actor, post, _, _ := generationSocial(t, store, root)
			job := contextJob(t, store, root, contextSocialChanges(actor, post, app.TriggerHumanPost))
			if test.setup != "" {
				generationSQL(t, store, test.setup)
			}
			generationSQL(t, store, test.query, post, test.value)
			if got, err := store.GenerationContext(context.Background(), job.ID, 1); !errors.Is(err, app.ErrGenerationOutput) || !reflect.DeepEqual(got, app.GenerationContext{}) {
				t.Fatalf("corruption: %v", err)
			}
		})
	}
}
