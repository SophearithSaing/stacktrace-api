package app

import (
	"encoding/json"
)

const (
	GenerationContextVersion    = "generation_context_v2"
	MaxGenerationContextReplies = 10
	MaxGenerationContextBytes   = 24 * 1024
)

// GenerationContextAuthor contains only bounded public identity, not account IDs,
// profile/private data or model-selected authority. Handles use ordinary rules.
type GenerationContextAuthor struct {
	Handle string      `json:"handle"`
	Type   AccountType `json:"type"`
}

// Kind describes existing public content, not a requested output/target. Storage
// derives it from the persisted post/quote/reply and verifies author and visibility.
type GenerationContextItem struct {
	Author  GenerationContextAuthor `json:"author"`
	Kind    GenerationOutput        `json:"kind"`
	Content Content                 `json:"content"`
}

// PublicGenerationContext is trusted storage input, never model-supplied targeting.
// Source is the job's canonical target post/quote. TriggerReply is fetched by the
// job's SourceReplyID independently of the recent window: it is always required
// when that ID exists, even if old. RepostActor identifies the bodyless public
// interaction separately from the source's author. No IDs enter the prompt.
// Storage must verify exact source/trigger/actor relations and visible authors;
// recent agent content must belong to the job's agent. Replies/recent content use
// oldest-first order with stable ID ties. This pure builder cannot prove those
// database facts. Attached source code is public data, never an executable tool.
type PublicGenerationContext struct {
	Source             *GenerationContextItem   `json:"source"`
	TriggerReply       *GenerationContextItem   `json:"trigger_reply"`
	RepostActor        *GenerationContextAuthor `json:"repost_actor"`
	Replies            []GenerationContextItem  `json:"replies"`
	RecentAgentContent []GenerationContextItem  `json:"recent_agent_content"`
}

// Immutable exact serialized context. Prompt/schema framing and the final
// context digest belong to the selected adapter's versioned prompt contract.
type GenerationContext struct {
	jobID          ID
	agentID        ID
	personaVersion int
	triggerKind    GenerationTrigger
	sourcePostID   ID
	sourceReplyID  ID
	sourceRepostID ID
	instructions   string
	publicJSON     string
	kind           GenerationOutput
}

func (c GenerationContext) Instructions() string         { return c.instructions }
func (c GenerationContext) PublicJSON() string           { return c.publicJSON }
func (c GenerationContext) OutputKind() GenerationOutput { return c.kind }
func (c GenerationContext) Matches(job GenerationJob) bool {
	return c.jobID == job.ID && c.agentID == job.AgentID && c.personaVersion == job.PersonaVersion &&
		c.kind == job.OutputKind && c.triggerKind == job.TriggerKind &&
		c.sourcePostID == generationContextID(job.SourcePostID) &&
		c.sourceReplyID == generationContextID(job.SourceReplyID) &&
		c.sourceRepostID == generationContextID(job.SourceRepostID) &&
		c.instructions != "" && c.publicJSON != ""
}

func BuildGenerationContext(job GenerationJob, persona Persona, public PublicGenerationContext) (GenerationContext, error) {
	if job.Validate() != nil || persona.Validate() != nil || persona.AgentID != job.AgentID || persona.Version != job.PersonaVersion {
		return GenerationContext{}, ErrGenerationOutput
	}
	if len(public.Replies) > MaxGenerationContextReplies || len(public.RecentAgentContent) > MaxGenerationRecentContent {
		return GenerationContext{}, ErrGenerationOutput
	}
	if (public.Source == nil) != (job.SourcePostID == nil) || (public.TriggerReply == nil) != (job.SourceReplyID == nil) || public.Source == nil && len(public.Replies) != 0 {
		return GenerationContext{}, ErrGenerationOutput
	}
	if (public.RepostActor != nil) != (job.TriggerKind == TriggerRepost) || job.TriggerKind == TriggerRepost && job.SourceRepostID == nil {
		return GenerationContext{}, ErrGenerationOutput
	}
	// Canonicalize using ordinary content rules; don't trust caller-provided tags.
	normalized := PublicGenerationContext{Replies: []GenerationContextItem{}, RecentAgentContent: []GenerationContextItem{}}
	interaction := "none"
	if public.Source != nil {
		source, err := normalizeGenerationContextItem(*public.Source)
		if err != nil || source.Kind == OutputReply || job.TriggerKind == TriggerHumanPost && source.Kind != OutputPost || job.TriggerKind == TriggerQuote && source.Kind != OutputQuote {
			return GenerationContext{}, ErrGenerationOutput
		}
		normalized.Source = &source
		interaction = string(source.Kind)
	}
	if public.TriggerReply != nil {
		reply, err := normalizeGenerationContextItem(*public.TriggerReply)
		if err != nil || reply.Kind != OutputReply {
			return GenerationContext{}, ErrGenerationOutput
		}
		normalized.TriggerReply = &reply
		interaction = "reply"
	}
	if public.RepostActor != nil {
		actor, err := normalizeGenerationContextAuthor(*public.RepostActor)
		if err != nil {
			return GenerationContext{}, err
		}
		normalized.RepostActor = &actor
		interaction = "repost"
	}
	for _, reply := range public.Replies {
		item, err := normalizeGenerationContextItem(reply)
		if err != nil || item.Kind != OutputReply {
			return GenerationContext{}, ErrGenerationOutput
		}
		normalized.Replies = append(normalized.Replies, item)
	}
	for _, recent := range public.RecentAgentContent {
		item, err := normalizeGenerationContextItem(recent)
		if err != nil || item.Author.Type != AccountAgent {
			return GenerationContext{}, ErrGenerationOutput
		}
		normalized.RecentAgentContent = append(normalized.RecentAgentContent, item)
	}
	// Interaction is derived from the job and verified public source kind, never
	// accepted as a caller-supplied override. Continuations expose only their public
	// post/quote/reply interaction, not private chain/scheduling metadata.
	encoded, err := json.Marshal(struct {
		Interaction string `json:"interaction"`
		PublicGenerationContext
	}{interaction, normalized})
	if err != nil || len(encoded)+len(persona.Instructions) > MaxGenerationContextBytes {
		return GenerationContext{}, ErrGenerationOutput
	}
	return GenerationContext{
		jobID: job.ID, agentID: job.AgentID, personaVersion: job.PersonaVersion, triggerKind: job.TriggerKind,
		sourcePostID: generationContextID(job.SourcePostID), sourceReplyID: generationContextID(job.SourceReplyID), sourceRepostID: generationContextID(job.SourceRepostID),
		instructions: persona.Instructions, publicJSON: string(encoded), kind: job.OutputKind,
	}, nil
}

func normalizeGenerationContextAuthor(author GenerationContextAuthor) (GenerationContextAuthor, error) {
	if len(author.Handle) > 32 {
		return GenerationContextAuthor{}, ErrGenerationOutput
	}
	handle, err := NormalizeHandle(author.Handle)
	if err != nil || author.Type != AccountHuman && author.Type != AccountAgent {
		return GenerationContextAuthor{}, ErrGenerationOutput
	}
	return GenerationContextAuthor{Handle: handle, Type: author.Type}, nil
}

func normalizeGenerationContextItem(item GenerationContextItem) (GenerationContextItem, error) {
	author, err := normalizeGenerationContextAuthor(item.Author)
	if err != nil || item.Kind != OutputPost && item.Kind != OutputQuote && item.Kind != OutputReply || item.Kind == OutputReply && item.Content.Code != nil {
		return GenerationContextItem{}, ErrGenerationOutput
	}
	content, err := NewContent(item.Content.Body, item.Content.Code)
	if err != nil || item.Kind != OutputReply && len(content.Tags) > MaxTagsPerPost {
		return GenerationContextItem{}, ErrGenerationOutput
	}
	return GenerationContextItem{Author: author, Kind: item.Kind, Content: content}, nil
}

func generationContextID(id *ID) ID {
	if id == nil {
		return ""
	}
	return *id
}
