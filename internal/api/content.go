package api

import (
	"fmt"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

type codeResponse struct {
	Language string `json:"language"`
	Filename string `json:"filename"`
	Source   string `json:"source"`
}

type tagResponse struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

type quoteResponse struct {
	ID           app.ID                  `json:"id"`
	Availability app.ContentAvailability `json:"availability"`
	Author       *accountSummary         `json:"author,omitempty"`
	Body         string                  `json:"body,omitempty"`
}

type reactionCountsResponse struct {
	Useful    int64 `json:"useful"`
	Agree     int64 `json:"agree"`
	Brilliant int64 `json:"brilliant"`
	Spicy     int64 `json:"spicy"`
	Ship      int64 `json:"ship"`
}

type postCountsResponse struct {
	Replies         int64                  `json:"replies"`
	Reposts         int64                  `json:"reposts"`
	ReactionsTotal  int64                  `json:"reactions_total"`
	ReactionsByKind reactionCountsResponse `json:"reactions_by_kind"`
}

type postViewerResponse struct {
	Reaction   *app.ReactionKind `json:"reaction"`
	Reposted   bool              `json:"reposted"`
	Bookmarked bool              `json:"bookmarked"`
}

type postResponse struct {
	ID           app.ID               `json:"id"`
	Author       accountSummary       `json:"author"`
	Body         string               `json:"body"`
	Tags         []tagResponse        `json:"tags"`
	Code         *codeResponse        `json:"code"`
	Quote        *quoteResponse       `json:"quote"`
	IsSpicy      bool                 `json:"is_spicy"`
	IsGenerated  bool                 `json:"is_generated"`
	CreatedAt    string               `json:"created_at"`
	Counts       postCountsResponse   `json:"counts"`
	Viewer       *postViewerResponse  `json:"viewer"`
	ReplyPreview replyPreviewResponse `json:"reply_preview"`
}

type replyResponse struct {
	ID          app.ID         `json:"id"`
	PostID      app.ID         `json:"post_id"`
	Author      accountSummary `json:"author"`
	Body        string         `json:"body"`
	IsGenerated bool           `json:"is_generated"`
	CreatedAt   string         `json:"created_at"`
}

type replyPreviewResponse struct {
	Items      []replyResponse `json:"items"`
	NextCursor *string         `json:"next_cursor"`
}

func replyDTO(reply app.Reply) replyResponse {
	return replyResponse{reply.ID, reply.PostID, summarizeAccount(reply.Author), reply.Body, reply.IsGenerated, formatTimestamp(reply.CreatedAt)}
}

func (s *server) postDTO(post app.Post) (postResponse, error) {
	tags := make([]tagResponse, 0, len(post.Content.Tags))
	for _, tag := range post.Content.Tags {
		tags = append(tags, tagResponse{tag.Slug, tag.DisplayName})
	}
	replies := make([]replyResponse, 0, len(post.ReplyPreview.Items))
	for _, reply := range post.ReplyPreview.Items {
		replies = append(replies, replyDTO(reply))
	}
	next, err := s.encodeCursor("replies", post.ID, "", app.ReplySortOldest, post.ReplyPreview.Ceiling, post.ReplyPreview.NextPosition)
	if err != nil {
		return postResponse{}, fmt.Errorf("encode preview cursor: %w", err)
	}
	response := postResponse{
		ID: post.ID, Author: summarizeAccount(post.Author), Body: post.Content.Body, Tags: tags,
		IsSpicy: post.IsSpicy, IsGenerated: post.IsGenerated, CreatedAt: formatTimestamp(post.CreatedAt),
		Counts:       postCountsResponse{post.Counts.Replies, post.Counts.Reposts, post.Counts.ReactionsTotal, reactionCountsResponse{post.Counts.Reactions.Useful, post.Counts.Reactions.Agree, post.Counts.Reactions.Brilliant, post.Counts.Reactions.Spicy, post.Counts.Reactions.Ship}},
		ReplyPreview: replyPreviewResponse{Items: replies, NextCursor: next},
	}
	if post.Content.Code != nil {
		response.Code = &codeResponse{post.Content.Code.Language, post.Content.Code.Filename, post.Content.Code.Source}
	}
	if post.Quote != nil {
		response.Quote = &quoteResponse{ID: post.Quote.ID, Availability: post.Quote.Availability}
		if post.Quote.Availability == app.ContentAvailable && post.Quote.Author != nil {
			author := summarizeAccount(*post.Quote.Author)
			response.Quote.Author, response.Quote.Body = &author, post.Quote.Body
		}
	}
	if post.Viewer != nil {
		response.Viewer = &postViewerResponse{post.Viewer.Reaction, post.Viewer.Reposted, post.Viewer.Bookmarked}
	}
	return response, nil
}

func formatTimestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
