package postgres

import (
	"context"
	"database/sql"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

// The publisher settings lock serializes its publications. Read the full bounded
// safety window here, not the possibly trimmed provider context snapshot.
func (q *Queries) publicationRecentContent(ctx context.Context, agent app.ID) ([]app.Content, error) {
	rows, err := q.queryer.QueryContext(ctx, `WITH recent AS (
		(SELECT p.id,p.created_at,p.body,p.code_language,p.code_filename,p.code_source,false AS is_reply
		 FROM posts p WHERE p.author_id=$1 AND p.deleted_at IS NULL ORDER BY p.created_at DESC,p.id DESC LIMIT $2)
		UNION ALL
		(SELECT r.id,r.created_at,r.body,NULL,NULL,NULL,true FROM replies r JOIN posts p ON p.id=r.post_id
		 JOIN accounts a ON a.id=p.author_id WHERE r.author_id=$1 AND r.deleted_at IS NULL AND p.deleted_at IS NULL
		 AND a.disabled_at IS NULL ORDER BY r.created_at DESC,r.id DESC LIMIT $2)
	) SELECT CASE WHEN octet_length(body)<=1280 THEN body END,
		CASE WHEN octet_length(code_language)<=32 THEN code_language END,
		CASE WHEN octet_length(code_filename)<=1020 THEN code_filename END,
		CASE WHEN octet_length(code_source)<=20480 THEN code_source END,
		COALESCE(octet_length(code_language)>32 OR octet_length(code_filename)>1020 OR octet_length(code_source)>20480,false)
		FROM recent ORDER BY created_at DESC,id DESC,is_reply DESC LIMIT $2`, agent, app.MaxGenerationRecentContent)
	if err != nil {
		return nil, databaseError(ctx, err)
	}
	defer rows.Close()
	var recent []app.Content
	for rows.Next() {
		var body, language, filename, source sql.NullString
		var oversized bool
		if err := rows.Scan(&body, &language, &filename, &source, &oversized); err != nil {
			return nil, databaseError(ctx, err)
		}
		if oversized || !body.Valid || language.Valid != filename.Valid || language.Valid != source.Valid {
			return nil, app.ErrGenerationOutput
		}
		var code *app.Code
		if language.Valid {
			code = &app.Code{Language: language.String, Filename: filename.String, Source: source.String}
		}
		content, err := app.NewContent(body.String, code)
		if err != nil {
			return nil, app.ErrGenerationOutput
		}
		recent = append(recent, content)
	}
	return recent, databaseError(ctx, rows.Err())
}
