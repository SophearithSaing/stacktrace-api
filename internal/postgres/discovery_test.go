package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func TestSearchPostsMatchesBodiesAndAuthors(t *testing.T) {
	store := feedTestStore(t)
	viewer, viewerSession := contentTestActor(t, store, "search_viewer")
	author, authorSession := contentTestActor(t, store, "search_author")
	body := feedPost(t, store, viewerSession, "search-body", "postgresql jsonb_path_ops")
	authorPost := feedPost(t, store, authorSession, "search-author", "ordinary content")
	feedExec(t, store, `UPDATE accounts SET display_name='100%_\search' WHERE id=$1`, author)
	page, err := store.SearchPosts(context.Background(), viewer, app.SearchQuery{Text: "jsonb_path_ops"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Post.ID != body.ID {
		t.Fatalf("body search = %#v, %v", page, err)
	}
	page, err = store.SearchPosts(context.Background(), viewer, app.SearchQuery{Text: "100%_\\search"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Post.ID != authorPost.ID {
		t.Fatalf("literal author search = %#v, %v", page, err)
	}
}

func TestSearchPostsPagination(t *testing.T) {
	store := feedTestStore(t)
	actor, session := contentTestActor(t, store, "search_paging")
	first := feedPost(t, store, session, "search-page-1", "searchable result one")
	second := feedPost(t, store, session, "search-page-2", "searchable result two")
	feedExec(t, store, `UPDATE posts SET created_at='2026-01-01T00:00:00Z' WHERE id IN ($1,$2)`, first.ID, second.ID)
	page, err := store.SearchPosts(context.Background(), actor, app.SearchQuery{Text: "searchable", Window: app.SearchWindow{Limit: 1}})
	if err != nil || len(page.Items) != 1 || page.NextPosition == nil {
		t.Fatalf("first page = %#v, %v", page, err)
	}
	next, err := store.SearchPosts(context.Background(), actor, app.SearchQuery{Text: "searchable", Window: app.SearchWindow{Limit: 1, Position: page.NextPosition, InitialCeiling: page.Ceiling}})
	if err != nil || len(next.Items) != 1 || next.Items[0].Post.ID == page.Items[0].Post.ID {
		t.Fatalf("second page = %#v, %v", next, err)
	}
	position := &app.SearchPosition{CreatedAt: next.Items[0].Post.CreatedAt, ID: next.Items[0].Post.ID}
	end, err := store.SearchPosts(context.Background(), actor, app.SearchQuery{Text: "searchable", Window: app.SearchWindow{Limit: 2, Position: position, InitialCeiling: page.Ceiling}})
	if err != nil || len(end.Items) != 0 || end.NextPosition != nil {
		t.Fatalf("end page = %#v, %v", end, err)
	}
}

func TestSearchPostsBoundariesAndDisabledAuthors(t *testing.T) {
	store := feedTestStore(t)
	ctx := context.Background()
	actor, session := contentTestActor(t, store, "search_boundaries")
	author, authorSession := contentTestActor(t, store, "search_matched_author")
	matched := feedPost(t, store, authorSession, "search-matched", "technical token jsonb_path_ops exact")
	feedExec(t, store, `UPDATE accounts SET display_name='jsonb_path_ops' WHERE id=$1`, author)
	code := feedPost(t, store, session, "search-code", "ordinary code post")
	feedExec(t, store, `UPDATE posts SET code_language='go',code_filename='main.go',code_source='private_code_marker' WHERE id=$1`, code.ID)
	replyTarget := feedPost(t, store, session, "search-reply-target", "ordinary reply target")
	feedExec(t, store, `INSERT INTO replies(id,post_id,author_id,body,created_at) VALUES($1,$2,$3,'private_reply_marker',statement_timestamp())`, app.NewID(), replyTarget.ID, actor)
	quotedSource := feedPost(t, store, session, "search-quoted-source", "private_quote_marker")
	quote := feedPost(t, store, session, "search-quote", "ordinary quote")
	feedExec(t, store, `UPDATE posts SET quoted_post_id=$1 WHERE id=$2`, quotedSource.ID, quote.ID)
	feedExec(t, store, `UPDATE posts SET deleted_at=statement_timestamp() WHERE id=$1`, quotedSource.ID)
	tagID := app.NewID()
	feedExec(t, store, `INSERT INTO tags(id,slug,display_name) VALUES($1,'private_tag_marker','private_tag_marker')`, tagID)
	feedExec(t, store, `INSERT INTO post_tags(post_id,tag_id) VALUES($1,$2)`, code.ID, tagID)
	if _, err := store.SetRepost(ctx, session, matched.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"private_code_marker", "private_reply_marker", "private_quote_marker", "private_tag_marker"} {
		page, err := store.SearchPosts(ctx, actor, app.SearchQuery{Text: text})
		if err != nil || len(page.Items) != 0 {
			t.Fatalf("excluded %q = %#v, %v", text, page, err)
		}
	}
	page, err := store.SearchPosts(ctx, actor, app.SearchQuery{Text: "ordinary quote"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Post.ID != quote.ID || page.Items[0].Post.Quote == nil || page.Items[0].Post.Quote.Availability != app.ContentDeleted {
		t.Fatalf("unavailable quote projection = %#v, %v", page, err)
	}
	page, err = store.SearchPosts(ctx, actor, app.SearchQuery{Text: "jsonb_path_ops"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Post.ID != matched.ID {
		t.Fatalf("body+author/repost result = %#v, %v", page, err)
	}
	feedExec(t, store, `UPDATE accounts SET disabled_at=statement_timestamp() WHERE id=$1`, author)
	page, err = store.SearchPosts(ctx, "", app.SearchQuery{Text: "jsonb_path_ops"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Post.ID != matched.ID {
		t.Fatalf("disabled author result = %#v, %v", page, err)
	}
}

func TestSearchPostsWebSearchSemanticsAndCeiling(t *testing.T) {
	store := feedTestStore(t)
	ctx := context.Background()
	actor, session := contentTestActor(t, store, "search_semantics")
	phrase := feedPost(t, store, session, "search-phrase", "postgres jsonb_path_ops")
	feedPost(t, store, session, "search-unrelated", "postgres ordinary")
	feedPost(t, store, session, "search-stem", "running only")
	page, err := store.SearchPosts(ctx, actor, app.SearchQuery{Text: `"postgres jsonb_path_ops" -ordinary`})
	if err != nil || len(page.Items) != 1 || page.Items[0].Post.ID != phrase.ID {
		t.Fatalf("phrase/negation = %#v, %v", page, err)
	}
	page, err = store.SearchPosts(ctx, actor, app.SearchQuery{Text: "run"})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("simple configuration stemmed = %#v, %v", page, err)
	}
	ceiling := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := feedPost(t, store, session, "search-old", "ceiling marker")
	newer := feedPost(t, store, session, "search-new", "ceiling marker")
	feedExec(t, store, `UPDATE posts SET created_at=$1 WHERE id=$2`, ceiling.Add(-time.Second), old.ID)
	feedExec(t, store, `UPDATE posts SET created_at=$1 WHERE id=$2`, ceiling.Add(time.Second), newer.ID)
	page, err = store.SearchPosts(ctx, actor, app.SearchQuery{Text: "ceiling marker", Window: app.SearchWindow{InitialCeiling: ceiling}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Post.ID != old.ID || !page.Ceiling.Equal(ceiling) {
		t.Fatalf("ceiling page = %#v, %v", page, err)
	}
}
