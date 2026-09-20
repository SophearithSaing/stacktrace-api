package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/seed"
)

func seedSnapshot(t *testing.T, store *Store) string {
	t.Helper()
	var snapshot string
	err := store.db.QueryRow(`SELECT jsonb_build_object(
		'accounts',(SELECT jsonb_agg(a ORDER BY id) FROM accounts a),
		'follows',(SELECT jsonb_agg(f ORDER BY follower_id,followed_id) FROM follows f),
		'personas',(SELECT jsonb_agg(p ORDER BY agent_id,version) FROM agent_personas p),
		'settings',(SELECT jsonb_agg(s ORDER BY agent_id) FROM agent_settings s),
		'jobs',(SELECT jsonb_agg(j ORDER BY id) FROM generation_jobs j),
		'attempts',(SELECT jsonb_agg(a ORDER BY id) FROM generation_attempts a))::text`).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func seededStore(t *testing.T) (*Store, demoFixture) {
	t.Helper()
	store := testStore(t)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture, err := decodeDemoFixture(seed.Demo, seed.Personas)
	if err != nil {
		t.Fatal(err)
	}
	return store, fixture
}

func TestSeedPersonasRepeatAndConcurrent(t *testing.T) {
	store, fixture := seededStore(t)
	ctx := context.Background()
	const workers = 8
	var group sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		group.Go(func() { errs <- store.SeedDemo(ctx) })
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, persona := range fixture.personas {
		got, err := store.PersonaByVersion(ctx, persona.AgentID, persona.Version)
		if err != nil || app.ValidatePersonaUnchanged(persona, got) != nil {
			t.Fatalf("persona: %v", err)
		}
		settings, err := store.AgentSettingsByID(ctx, persona.AgentID)
		if err != nil || settings.Enabled || settings.Policy != fixture.settings[i].Policy || !settings.UpdatedAt.Equal(persona.CreatedAt) {
			t.Fatalf("initial settings: %+v %v", settings, err)
		}
	}
	want := seedSnapshot(t, store)
	if err := store.SeedDemo(ctx); err != nil {
		t.Fatal(err)
	}
	if got := seedSnapshot(t, store); got != want {
		t.Fatal("repeat seed changed persisted state")
	}
}

func TestSeedPersonasPreserveOperatorState(t *testing.T) {
	store, fixture := seededStore(t)
	ctx := context.Background()
	if err := store.SeedDemo(ctx); err != nil {
		t.Fatal(err)
	}
	for i, persona := range fixture.personas {
		persona.Version, persona.Instructions = 2, "Operator-authored version two."
		if err := store.CreatePersona(ctx, persona); err != nil {
			t.Fatal(err)
		}
		generationSQL(t, store, `UPDATE agent_settings SET persona_version=2,enabled=$2,
			policy=jsonb_set(policy,'{daily_token_budget}','12345'),next_post_at='2026-09-21 10:00:00Z',
			schedule_date='2026-09-21',remaining_slots=2,last_published_at='2026-09-20 10:00:00Z',updated_at='2026-09-20 11:00:00Z'
			WHERE agent_id=$1`, persona.AgentID, i == 0)
		generationSQL(t, store, `UPDATE accounts SET display_name='Operator name',bio='Operator bio',updated_at='2027-01-01' WHERE id=$1`, persona.AgentID)
	}
	want := seedSnapshot(t, store)
	if err := store.SeedDemo(ctx); err != nil {
		t.Fatal(err)
	}
	if got := seedSnapshot(t, store); got != want {
		t.Fatal("seed overwrote selected v2, enabled/paused state, policy, schedule, timestamps or profile")
	}
}

func TestSeedPersonasConflictRollsBack(t *testing.T) {
	for _, kind := range []string{"instructions", "topics", "creation time", "human", "disabled", "handle", "handle collision", "invalid selected persona", "invalid policy"} {
		t.Run(kind, func(t *testing.T) {
			store, fixture := seededStore(t)
			ctx := context.Background()
			// Only the second account exists. All new first-account, follow,
			// persona and settings rows must roll back on the later conflict.
			account := fixture.accounts[1]
			switch kind {
			case "human":
				account.Type = app.AccountHuman
			case "disabled":
				account.DisabledAt = &account.CreatedAt
			case "handle":
				account.Handle = "changed_handle"
			case "handle collision":
				account.ID = app.NewID()
			}
			if err := store.CreateAccount(ctx, account); err != nil {
				t.Fatal(err)
			}
			if kind == "disabled" {
				generationSQL(t, store, `UPDATE accounts SET disabled_at=created_at WHERE id=$1`, account.ID)
			}
			persona := fixture.personas[1]
			wantErr := app.ErrConflict
			switch kind {
			case "instructions", "topics", "creation time":
				switch kind {
				case "instructions":
					persona.Instructions = "Different immutable instructions."
				case "topics":
					persona.TopicTags = []string{"different"}
				case "creation time":
					persona.CreatedAt = persona.CreatedAt.AddDate(0, 0, 1)
				}
				if err := store.CreatePersona(ctx, persona); err != nil {
					t.Fatal(err)
				}
			case "invalid selected persona", "invalid policy":
				if err := store.CreatePersona(ctx, persona); err != nil {
					t.Fatal(err)
				}
				if _, err := store.InitializeAgentSettings(ctx, fixture.settings[1]); err != nil {
					t.Fatal(err)
				}
				if kind == "invalid selected persona" {
					generationSQL(t, store, `INSERT INTO agent_personas VALUES($1,2,'private instructions',ARRAY['Bad Tag'],now())`, persona.AgentID)
					generationSQL(t, store, `UPDATE agent_settings SET persona_version=2 WHERE agent_id=$1`, persona.AgentID)
				} else {
					generationSQL(t, store, `UPDATE agent_settings SET policy='{"version":1,"secret":"private"}' WHERE agent_id=$1`, persona.AgentID)
				}
				wantErr = app.ErrUnavailable
			}
			want := seedSnapshot(t, store)
			if err := store.SeedDemo(ctx); !errors.Is(err, wantErr) {
				t.Fatalf("got %v, want %v", err, wantErr)
			}
			if got := seedSnapshot(t, store); got != want {
				t.Fatal("failed seed changed persisted state")
			}
		})
	}
}
