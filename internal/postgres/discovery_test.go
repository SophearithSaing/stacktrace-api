package postgres

import (
	"context"
	"testing"

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
}
