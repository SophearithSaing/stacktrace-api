package llm

import (
	"strings"
	"testing"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
)

func socialPromptFixture(t *testing.T) (app.GenerationJob, app.Persona, app.PublicGenerationContext) {
	t.Helper()
	job := generationRequest(t, "Discuss Go.").Job
	actor, source, reply := app.NewID(), app.NewID(), app.NewID()
	job.TriggerKind, job.OutputKind, job.TriggerActorID, job.SourcePostID, job.SourceReplyID, job.CooldownKey = app.TriggerReply, app.OutputReply, &actor, &source, &reply, "private_cooldown"
	job.TriggerKey = "private_trigger"
	persona := app.Persona{AgentID: job.AgentID, Version: job.PersonaVersion, Instructions: "Discuss Go.", TopicTags: []string{"go"}, CreatedAt: job.CreatedAt}
	public := app.PublicGenerationContext{
		Source:             &app.GenerationContextItem{Author: app.GenerationContextAuthor{Handle: "source_author", Type: app.AccountHuman}, Kind: app.OutputPost, Content: app.Content{Body: "The source post."}},
		TriggerReply:       &app.GenerationContextItem{Author: app.GenerationContextAuthor{Handle: "trigger_author", Type: app.AccountHuman}, Kind: app.OutputReply, Content: app.Content{Body: "The triggering reply."}},
		Replies:            []app.GenerationContextItem{{Author: app.GenerationContextAuthor{Handle: "recent_author", Type: app.AccountHuman}, Kind: app.OutputReply, Content: app.Content{Body: "A newer reply."}}},
		RecentAgentContent: []app.GenerationContextItem{{Author: app.GenerationContextAuthor{Handle: "go_agent", Type: app.AccountAgent}, Kind: app.OutputPost, Content: app.Content{Body: "An earlier agent post."}}},
	}
	return job, persona, public
}

func buildSocialPrompt(t *testing.T, job app.GenerationJob, persona app.Persona, public app.PublicGenerationContext) Prompt {
	t.Helper()
	context, err := app.BuildGenerationContext(job, persona, public)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := BuildPrompt(app.GenerationRequest{Job: job, Context: context})
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

func TestGenerationPromptPublicContextHash(t *testing.T) {
	job, persona, public := socialPromptFixture(t)
	base := buildSocialPrompt(t, job, persona, public)
	for name, change := range map[string]func(*app.GenerationJob, *app.PublicGenerationContext){
		"source body":         func(_ *app.GenerationJob, p *app.PublicGenerationContext) { p.Source.Content.Body = "Changed source." },
		"source author":       func(_ *app.GenerationJob, p *app.PublicGenerationContext) { p.Source.Author.Handle = "another_author" },
		"source account type": func(_ *app.GenerationJob, p *app.PublicGenerationContext) { p.Source.Author.Type = app.AccountAgent },
		"source quote":        func(_ *app.GenerationJob, p *app.PublicGenerationContext) { p.Source.Kind = app.OutputQuote },
		"trigger body": func(_ *app.GenerationJob, p *app.PublicGenerationContext) {
			p.TriggerReply.Content.Body = "Different triggering reply."
		},
		"trigger author": func(_ *app.GenerationJob, p *app.PublicGenerationContext) {
			p.TriggerReply.Author.Handle = "another_author"
		},
		"recent author": func(_ *app.GenerationJob, p *app.PublicGenerationContext) {
			p.Replies[0].Author.Handle = "another_author"
		},
		"recent account type": func(_ *app.GenerationJob, p *app.PublicGenerationContext) {
			p.Replies[0].Author.Type = app.AccountAgent
		},
		"agent content": func(_ *app.GenerationJob, p *app.PublicGenerationContext) {
			p.RecentAgentContent[0].Content.Body = "Different prior content."
		},
		"code": func(_ *app.GenerationJob, p *app.PublicGenerationContext) {
			p.Source.Content.Code = &app.Code{Language: "go", Filename: "a.go", Source: "x"}
		},
		"repost": func(j *app.GenerationJob, p *app.PublicGenerationContext) {
			id := app.NewID()
			j.TriggerKind = app.TriggerRepost
			j.SourceReplyID = nil
			j.SourceRepostID = &id
			p.TriggerReply = nil
			p.RepostActor = &app.GenerationContextAuthor{Handle: "reposting_author", Type: app.AccountHuman}
		},
	} {
		t.Run(name, func(t *testing.T) {
			changedJob, changedPersona, changedPublic := socialPromptFixture(t)
			change(&changedJob, &changedPublic)
			changed := buildSocialPrompt(t, changedJob, changedPersona, changedPublic)
			if changed.Hash() == base.Hash() || changed.User() == base.User() {
				t.Fatal("public change not bound")
			}
		})
	}
	// Identical public content in a continuation must not reveal chain/queue
	// identity, nor hash hidden metadata that was never sent to the provider.
	job.TriggerKind = app.TriggerContinuation
	job.RootJobID, job.ChainDepth = app.NewID(), 1
	job.MaxChainDepth, job.MaxChainJobs = 1, 2
	continuation := buildSocialPrompt(t, job, persona, public)
	if continuation.Hash() != base.Hash() || continuation.User() != base.User() {
		t.Fatal("private continuation metadata changed prompt")
	}
	for _, private := range []string{string(job.ID), string(job.AgentID), string(job.RootJobID), string(*job.SourceReplyID), "private_cooldown", "private_trigger", "continuation", "chain_depth", "lease_version"} {
		if strings.Contains(base.System()+base.User(), private) || strings.Contains(continuation.System()+continuation.User(), private) {
			t.Fatal("private metadata in prompt", private)
		}
	}
	if !strings.Contains(base.User(), `"trigger_reply"`) || !strings.Contains(base.User(), "trigger_author") || !strings.Contains(base.User(), "recent_author") {
		t.Fatal("missing distinct trigger/speaker context")
	}
	if strings.Contains(base.Schema(), `"code"`) {
		t.Fatal("reply schema allows code")
	}
}
