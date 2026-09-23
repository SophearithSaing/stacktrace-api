package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func contextItem(kind GenerationOutput, handle string, accountType AccountType, body string) GenerationContextItem {
	return GenerationContextItem{Author: GenerationContextAuthor{Handle: handle, Type: accountType}, Kind: kind, Content: Content{Body: body}}
}

func contextFixture(t *testing.T, trigger GenerationTrigger, sourceKind GenerationOutput, triggerReply bool) (GenerationJob, Persona, PublicGenerationContext) {
	t.Helper()
	job := testGenerationJob()
	public := PublicGenerationContext{}
	if trigger != TriggerScheduled {
		job = generationOutputJob(OutputReply)
		job.TriggerKind = trigger
		source := contextItem(sourceKind, "source_author", AccountHuman, "The source post.")
		public.Source = &source
		if triggerReply {
			id := NewID()
			job.SourceReplyID = &id
			reply := contextItem(OutputReply, "reply_author", AccountHuman, "An older triggering reply.")
			public.TriggerReply = &reply
		}
		if trigger == TriggerRepost {
			id := NewID()
			job.SourceRepostID = &id
			public.RepostActor = &GenerationContextAuthor{Handle: "repost_actor", Type: AccountHuman}
		}
		if trigger == TriggerContinuation {
			job.RootJobID, job.ChainDepth = NewID(), 1
			if triggerReply {
				public.TriggerReply.Author.Type = AccountAgent
			} else {
				public.Source.Author.Type = AccountAgent
			}
		}
	}
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	persona := Persona{AgentID: job.AgentID, Version: job.PersonaVersion, Instructions: "Discuss Go.", TopicTags: []string{"go"}, CreatedAt: job.CreatedAt}
	return job, persona, public
}

func TestGenerationContextSocialShapes(t *testing.T) {
	for _, test := range []struct {
		trigger     GenerationTrigger
		sourceKind  GenerationOutput
		reply       bool
		interaction string
	}{
		{TriggerScheduled, OutputPost, false, "none"},
		{TriggerHumanPost, OutputPost, false, "post"},
		{TriggerReply, OutputPost, true, "reply"},
		{TriggerReply, OutputQuote, true, "reply"},
		{TriggerRepost, OutputPost, false, "repost"},
		{TriggerRepost, OutputQuote, false, "repost"},
		{TriggerQuote, OutputQuote, false, "quote"},
		{TriggerContinuation, OutputPost, false, "post"},
		{TriggerContinuation, OutputQuote, false, "quote"},
		{TriggerContinuation, OutputPost, true, "reply"},
	} {
		t.Run(string(test.trigger)+"_"+string(test.sourceKind)+"_"+test.interaction, func(t *testing.T) {
			job, persona, public := contextFixture(t, test.trigger, test.sourceKind, test.reply)
			if public.Source != nil {
				for range MaxGenerationContextReplies {
					public.Replies = append(public.Replies, contextItem(OutputReply, "recent_author", AccountHuman, "A newer reply."))
				}
			}
			built, err := BuildGenerationContext(job, persona, public)
			if err != nil || !built.Matches(job) || built.Instructions() != persona.Instructions {
				t.Fatal("invalid context", err)
			}
			var decoded struct {
				Interaction string `json:"interaction"`
				PublicGenerationContext
			}
			if err := json.Unmarshal([]byte(built.PublicJSON()), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Interaction != test.interaction {
				t.Fatal("wrong interaction", decoded.Interaction)
			}
			if test.reply && (decoded.TriggerReply == nil || decoded.TriggerReply.Content.Body != "An older triggering reply." || decoded.TriggerReply.Author.Handle != "reply_author") {
				t.Fatal("lost triggering reply outside recent window")
			}
			if test.trigger == TriggerRepost && (decoded.RepostActor == nil || decoded.RepostActor.Handle == decoded.Source.Author.Handle) {
				t.Fatal("lost distinct repost actor")
			}
			if public.Source != nil && decoded.Source.Kind != test.sourceKind {
				t.Fatal("lost source kind")
			}
			for _, private := range []string{string(job.ID), string(job.AgentID), string(job.RootJobID), "trigger_kind", "chain_depth", "persona_version", "lease_version", "scheduled", "continuation"} {
				if strings.Contains(built.PublicJSON(), private) {
					t.Fatal("private metadata in public context", private)
				}
			}
		})
	}
}

func TestGenerationContextRejectsWrongShapes(t *testing.T) {
	for _, test := range []struct {
		name       string
		trigger    GenerationTrigger
		sourceKind GenerationOutput
		reply      bool
		change     func(*GenerationJob, *PublicGenerationContext)
	}{
		{"missing source", TriggerHumanPost, OutputPost, false, func(_ *GenerationJob, p *PublicGenerationContext) { p.Source = nil }},
		{"source reply", TriggerHumanPost, OutputPost, false, func(_ *GenerationJob, p *PublicGenerationContext) { p.Source.Kind = OutputReply }},
		{"original is quote", TriggerHumanPost, OutputPost, false, func(_ *GenerationJob, p *PublicGenerationContext) { p.Source.Kind = OutputQuote }},
		{"quote is original", TriggerQuote, OutputQuote, false, func(_ *GenerationJob, p *PublicGenerationContext) { p.Source.Kind = OutputPost }},
		{"unknown kind", TriggerHumanPost, OutputPost, false, func(_ *GenerationJob, p *PublicGenerationContext) { p.Source.Kind = "invented" }},
		{"empty body", TriggerHumanPost, OutputPost, false, func(_ *GenerationJob, p *PublicGenerationContext) { p.Source.Content.Body = "" }},
		{"missing trigger", TriggerReply, OutputPost, true, func(_ *GenerationJob, p *PublicGenerationContext) {
			p.Replies = []GenerationContextItem{*p.TriggerReply}
			p.TriggerReply = nil
		}},
		{"missing continuation trigger", TriggerContinuation, OutputPost, true, func(_ *GenerationJob, p *PublicGenerationContext) { p.TriggerReply = nil }},
		{"wrong trigger type", TriggerReply, OutputPost, true, func(_ *GenerationJob, p *PublicGenerationContext) { p.TriggerReply.Kind = OutputPost }},
		{"extra trigger", TriggerHumanPost, OutputPost, false, func(_ *GenerationJob, p *PublicGenerationContext) {
			item := contextItem(OutputReply, "someone", AccountHuman, "hi")
			p.TriggerReply = &item
		}},
		{"missing repost actor", TriggerRepost, OutputPost, false, func(_ *GenerationJob, p *PublicGenerationContext) { p.RepostActor = nil }},
		{"removed repost", TriggerRepost, OutputPost, false, func(j *GenerationJob, _ *PublicGenerationContext) { j.SourceRepostID = nil }},
		{"extra repost actor", TriggerHumanPost, OutputPost, false, func(_ *GenerationJob, p *PublicGenerationContext) { p.RepostActor = &p.Source.Author }},
		{"wrong recent reply kind", TriggerReply, OutputPost, true, func(_ *GenerationJob, p *PublicGenerationContext) { p.Replies = []GenerationContextItem{*p.Source} }},
		{"human recent agent content", TriggerHumanPost, OutputPost, false, func(_ *GenerationJob, p *PublicGenerationContext) {
			p.RecentAgentContent = []GenerationContextItem{*p.Source}
		}},
		{"trigger code", TriggerReply, OutputPost, true, func(_ *GenerationJob, p *PublicGenerationContext) {
			p.TriggerReply.Content.Code = &Code{Language: "go", Filename: "a.go", Source: "x"}
		}},
		{"scheduled source", TriggerScheduled, OutputPost, false, func(_ *GenerationJob, p *PublicGenerationContext) {
			item := contextItem(OutputPost, "someone", AccountHuman, "hi")
			p.Source = &item
		}},
		{"scheduled replies", TriggerScheduled, OutputPost, false, func(_ *GenerationJob, p *PublicGenerationContext) {
			p.Replies = []GenerationContextItem{contextItem(OutputReply, "someone", AccountHuman, "hi")}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			job, persona, public := contextFixture(t, test.trigger, test.sourceKind, test.reply)
			test.change(&job, &public)
			if _, err := BuildGenerationContext(job, persona, public); err == nil {
				t.Fatal("invalid shape accepted")
			}
		})
	}
}

func TestGenerationContextAuthors(t *testing.T) {
	for _, test := range []struct {
		handle string
		valid  bool
	}{
		{"", false}, {"ab", false}, {"ABC", true}, {" AbC ", true}, {strings.Repeat("a", 32), true}, {strings.Repeat("a", 33), false},
		{"@abc", false}, {"first.last", false}, {"a\x00b", false}, {"abc界", false}, {"\xffabc", false}, {strings.Repeat(" ", 100) + "abc", false},
	} {
		for _, field := range []string{"source", "trigger", "reply", "recent", "repost"} {
			trigger, needsReply := TriggerReply, true
			if field == "repost" {
				trigger, needsReply = TriggerRepost, false
			}
			job, persona, public := contextFixture(t, trigger, OutputPost, needsReply)
			switch field {
			case "source":
				public.Source.Author.Handle = test.handle
			case "trigger":
				public.TriggerReply.Author.Handle = test.handle
			case "reply":
				public.Replies = []GenerationContextItem{contextItem(OutputReply, test.handle, AccountHuman, "hi")}
			case "recent":
				public.RecentAgentContent = []GenerationContextItem{contextItem(OutputPost, test.handle, AccountAgent, "hi")}
			case "repost":
				public.RepostActor.Handle = test.handle
			}
			built, err := BuildGenerationContext(job, persona, public)
			if (err == nil) != test.valid {
				t.Fatalf("%s author %q: %v", field, test.handle, err)
			}
			if test.valid && !strings.Contains(built.PublicJSON(), `"handle":"`+strings.ToLower(strings.TrimSpace(test.handle))+`"`) {
				t.Fatal("author not normalized")
			}
		}
	}
	job, persona, public := contextFixture(t, TriggerHumanPost, OutputPost, false)
	for _, accountType := range []AccountType{"", "admin", "Human"} {
		public.Source.Author.Type = accountType
		if _, err := BuildGenerationContext(job, persona, public); err == nil {
			t.Fatal("invalid account type")
		}
	}
}

func TestGenerationContextBounds(t *testing.T) {
	job, persona, public := contextFixture(t, TriggerReply, OutputPost, true)
	for range MaxGenerationContextReplies {
		public.Replies = append(public.Replies, contextItem(OutputReply, "recent_human", AccountHuman, "A reply."))
	}
	for range MaxGenerationRecentContent {
		public.RecentAgentContent = append(public.RecentAgentContent, contextItem(OutputPost, "go_agent", AccountAgent, "An agent post."))
	}
	if _, err := BuildGenerationContext(job, persona, public); err != nil {
		t.Fatal("exact row ceilings rejected", err)
	}
	tooMany := public
	tooMany.Replies = append(append([]GenerationContextItem{}, public.Replies...), public.Replies[0])
	if _, err := BuildGenerationContext(job, persona, tooMany); err == nil {
		t.Fatal("reply ceiling exceeded")
	}
	tooMany = public
	tooMany.RecentAgentContent = append(append([]GenerationContextItem{}, public.RecentAgentContent...), public.RecentAgentContent[0])
	if _, err := BuildGenerationContext(job, persona, tooMany); err == nil {
		t.Fatal("recent ceiling exceeded")
	}
	job, persona, public = contextFixture(t, TriggerHumanPost, OutputPost, false)
	public.Source.Content.Code = &Code{Language: "go", Filename: "a.go", Source: strings.Repeat(`"`, 10000)}
	base, err := BuildGenerationContext(job, persona, public)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{-1, 0, 1} {
		// Include multibyte persona text and the actual JSON escaping/author/
		// interaction overhead, not just raw source body lengths.
		persona.Instructions = "界" + strings.Repeat("x", MaxGenerationContextBytes-len(base.PublicJSON())-3+delta)
		built, err := BuildGenerationContext(job, persona, public)
		if (err == nil) != (delta <= 0) {
			t.Fatalf("24KiB delta %d: %v", delta, err)
		}
		if err == nil && len(built.PublicJSON())+len(built.Instructions()) != MaxGenerationContextBytes+delta {
			t.Fatal("wrong byte ceiling")
		}
	}
}

func TestGenerationContextImmutableAndPinned(t *testing.T) {
	job, persona, public := contextFixture(t, TriggerReply, OutputPost, true)
	public.Source.Content = Content{Body: " Source #Go ", Tags: []Tag{{Slug: "untrusted"}}, Code: &Code{Language: " go ", Filename: " a.go ", Source: " x "}}
	public.Replies = []GenerationContextItem{contextItem(OutputReply, "reader", AccountHuman, "Recent reply")}
	public.RecentAgentContent = []GenerationContextItem{contextItem(OutputPost, "go_agent", AccountAgent, "An agent post")}
	first, err := BuildGenerationContext(job, persona, public)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildGenerationContext(job, persona, public)
	if err != nil || first != second {
		t.Fatal("nondeterministic context", err)
	}
	if strings.Contains(first.PublicJSON(), "untrusted") || !strings.Contains(first.PublicJSON(), `"Slug":"go"`) {
		t.Fatal("caller tags were not derived")
	}
	public.Source.Author.Handle = "mutated"
	public.Source.Content.Code.Source = "mutated"
	public.Source.Content.Tags[0].Slug = "mutated"
	public.TriggerReply.Content.Body = "mutated"
	public.Replies[0].Content.Body = "mutated"
	public.RecentAgentContent[0].Author.Handle = "mutated"
	if first.PublicJSON() != second.PublicJSON() || strings.Contains(first.PublicJSON(), "mutated") {
		t.Fatal("serialized context mutated")
	}
	for _, change := range []func(*GenerationJob){
		func(j *GenerationJob) { j.ID = NewID() }, func(j *GenerationJob) { j.AgentID = NewID() }, func(j *GenerationJob) { j.PersonaVersion++ },
		func(j *GenerationJob) { id := NewID(); j.SourcePostID = &id }, func(j *GenerationJob) { id := NewID(); j.SourceReplyID = &id },
	} {
		other := job
		change(&other)
		if first.Matches(other) {
			t.Fatal("context matches different authority")
		}
	}
	persona.Version++
	if _, err := BuildGenerationContext(job, persona, public); err == nil {
		t.Fatal("unpinned persona accepted")
	}
	job, persona, public = contextFixture(t, TriggerScheduled, OutputPost, false)
	nilSlices, err := BuildGenerationContext(job, persona, public)
	if err != nil {
		t.Fatal(err)
	}
	public.Replies = []GenerationContextItem{}
	public.RecentAgentContent = []GenerationContextItem{}
	emptySlices, err := BuildGenerationContext(job, persona, public)
	if err != nil || nilSlices != emptySlices {
		t.Fatal("nil/empty context differs", err)
	}
}
