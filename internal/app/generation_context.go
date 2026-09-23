package app

import (
	"encoding/json"
)

const (
	GenerationContextVersion    = "generation_context_v1"
	MaxGenerationContextReplies = 10
	MaxGenerationContextBytes   = 24 * 1024
)

// PublicGenerationContext must be populated from a bounded visible snapshot.
// Source is the canonical target post; a source reply is included among Replies.
// Replies and RecentAgentContent are oldest-first with stable ID ties. Storage
// must establish visibility/authorship/order; this pure builder cannot do so.
// Attached source code is public data, never a tool or executable instruction.
type PublicGenerationContext struct {
	Source             *Content  `json:"source"`
	Replies            []Content `json:"replies"`
	RecentAgentContent []Content `json:"recent_agent_content"`
}

// Immutable exact serialized context. Prompt/schema framing and the final
// context digest belong to the selected adapter's versioned prompt contract.
type GenerationContext struct {
	jobID        ID
	instructions string
	publicJSON   string
	kind         GenerationOutput
}

func (c GenerationContext) Instructions() string         { return c.instructions }
func (c GenerationContext) PublicJSON() string           { return c.publicJSON }
func (c GenerationContext) OutputKind() GenerationOutput { return c.kind }
func (c GenerationContext) Matches(job GenerationJob) bool {
	return c.jobID == job.ID && c.kind == job.OutputKind && c.instructions != "" && c.publicJSON != ""
}

func BuildGenerationContext(job GenerationJob, persona Persona, public PublicGenerationContext) (GenerationContext, error) {
	if job.Validate() != nil || persona.Validate() != nil || persona.AgentID != job.AgentID || persona.Version != job.PersonaVersion || len(public.Replies) > MaxGenerationContextReplies || len(public.RecentAgentContent) > MaxGenerationRecentContent || (public.Source == nil) != (job.SourcePostID == nil) || public.Source == nil && len(public.Replies) != 0 {
		return GenerationContext{}, ErrGenerationOutput
	}
	// Canonicalize using ordinary content rules; don't trust caller-provided tags.
	normalized := PublicGenerationContext{Replies: []Content{}, RecentAgentContent: []Content{}}
	if public.Source != nil {
		source, err := NewContent(public.Source.Body, public.Source.Code)
		if err != nil {
			return GenerationContext{}, ErrGenerationOutput
		}
		normalized.Source = &source
	}
	for _, reply := range public.Replies {
		if reply.Code != nil {
			return GenerationContext{}, ErrGenerationOutput
		}
		content, err := NewContent(reply.Body, nil)
		if err != nil {
			return GenerationContext{}, ErrGenerationOutput
		}
		normalized.Replies = append(normalized.Replies, content)
	}
	for _, recent := range public.RecentAgentContent {
		content, err := NewContent(recent.Body, recent.Code)
		if err != nil {
			return GenerationContext{}, ErrGenerationOutput
		}
		normalized.RecentAgentContent = append(normalized.RecentAgentContent, content)
	}
	encoded, err := json.Marshal(normalized)
	if err != nil || len(encoded)+len(persona.Instructions) > MaxGenerationContextBytes {
		return GenerationContext{}, ErrGenerationOutput
	}
	return GenerationContext{jobID: job.ID, instructions: persona.Instructions, publicJSON: string(encoded), kind: job.OutputKind}, nil
}
