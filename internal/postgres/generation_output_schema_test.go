package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/migrations"
)

func TestGenerationSchemaOutputDigest(t *testing.T) {
	store, _, job := generationSetup(t)
	id := generationAttempt(t, store, job, 1)
	digest := app.GenerationOutputDigestVersion + ":" + strings.Repeat("a", 64)
	for _, invalid := range []string{"", strings.Repeat("a", 64), "generation_output_v2:" + strings.Repeat("a", 64), strings.ToUpper(digest), digest + "\n", digest + "a"} {
		generationReject(t, store, "23514", `UPDATE generation_attempts SET status='succeeded',finished_at=started_at,output_digest=$2 WHERE id=$1`, id, invalid)
	}
	generationReject(t, store, "23514", `UPDATE generation_attempts SET output_digest=$2 WHERE id=$1`, id, digest)
	for _, status := range []string{"failed", "unknown"} {
		generationReject(t, store, "23514", `UPDATE generation_attempts SET status=$2,error_code='test',finished_at=started_at,output_digest=$3 WHERE id=$1`, id, status, digest)
	}
	generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=started_at,output_digest=$2 WHERE id=$1`, id, digest)
	attempt, err := store.GenerationAttemptByID(context.Background(), id)
	if err != nil || attempt.OutputDigest != digest {
		t.Fatalf("digest round trip: %+v %v", attempt, err)
	}
	for _, change := range []string{"NULL", "'generation_output_v1:'||repeat('b',64)"} {
		generationReject(t, store, "23514", `UPDATE generation_attempts SET output_digest=`+change+` WHERE id=$1`, id)
	}
	generationReject(t, store, "23514", `DELETE FROM generation_attempts WHERE id=$1`, id)
}

func TestMigrateGenerationOutputFromScheduling(t *testing.T) {
	store, root := schedulingLegacySetup(t)
	ctx := context.Background()
	catalog, err := loadMigrations(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx, catalog[:4]); err != nil {
		t.Fatal(err)
	}
	root, err = store.GenerationJobByID(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy := generationAttempt(t, store, root, 1)
	reserved := generationAttempt(t, store, root, 2)
	failed := generationAttempt(t, store, root, 3)
	unknown := generationAttempt(t, store, root, 4)
	post := app.NewID()
	generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=started_at WHERE id=$1`, legacy)
	generationSQL(t, store, `UPDATE generation_attempts SET status='failed',error_code='provider_failure',finished_at=started_at WHERE id=$1`, failed)
	generationSQL(t, store, `UPDATE generation_attempts SET status='unknown',error_code='provider_timeout',finished_at=started_at WHERE id=$1`, unknown)
	generationSQL(t, store, `INSERT INTO posts(id,author_id,body,created_at) VALUES($1,$2,'Legacy publication',$3)`, post, root.AgentID, root.CreatedAt)
	generationSQL(t, store, `UPDATE generation_jobs SET status='succeeded',lease_version=1,result_post_id=$2,published_attempt_id=$3,finished_at=created_at WHERE id=$1`, root.ID, post, legacy)
	var attemptsBefore, jobsBefore, attemptsAfter, jobsAfter string
	if err := store.db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM generation_attempts a`).Scan(&attemptsBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(j) ORDER BY id)::text FROM generation_jobs j`).Scan(&jobsBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(ctx); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("004 readiness: %v", err)
	}
	for range 2 {
		if err := store.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(a)-'output_digest' ORDER BY id)::text FROM generation_attempts a`).Scan(&attemptsAfter); err != nil || attemptsAfter != attemptsBefore {
		t.Fatal("attempt history rewritten", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT jsonb_agg(to_jsonb(j) ORDER BY id)::text FROM generation_jobs j`).Scan(&jobsAfter); err != nil || jobsAfter != jobsBefore {
		t.Fatal("publication provenance rewritten", err)
	}
	for _, id := range []app.ID{legacy, reserved, failed, unknown} {
		attempt, err := store.GenerationAttemptByID(ctx, id)
		if err != nil || attempt.OutputDigest != "" {
			t.Fatalf("legacy attempt unreadable or invented digest: %+v %v", attempt, err)
		}
		if id != reserved {
			generationReject(t, store, "23514", `UPDATE generation_attempts SET output_digest='generation_output_v1:'||repeat('a',64) WHERE id=$1`, id)
		}
	}
	saved, err := store.GenerationJobByID(ctx, root.ID)
	if err != nil || saved.Status != app.JobSucceeded || saved.PublishedAttemptID == nil || *saved.PublishedAttemptID != legacy {
		t.Fatal("legacy result lost", err)
	}
	generationReject(t, store, "23514", `UPDATE generation_jobs SET published_attempt_id=$2 WHERE id=$1`, root.ID, reserved)
	// A pre-migration reserved attempt can finish with a real validated digest.
	generationSQL(t, store, `UPDATE generation_attempts SET status='succeeded',finished_at=started_at,output_digest='generation_output_v1:'||repeat('a',64) WHERE id=$1`, reserved)
}
