package app

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxGenerationMentions = 32

// GenerationMentions extracts bounded, unique normalized handles in appearance
// order. Callers must resolve these against known eligible agents; text alone
// cannot identify an account. Parse only the plain body, never attached code.
// Later SQL candidate priority is source/parent author, mentions, then tags.
func GenerationMentions(body string) []string {
	if len(body) > MaxBodyRunes*utf8.UTFMax || !utf8.ValidString(body) {
		return nil
	}
	var handles []string
	seen := make(map[string]bool)
	for i := 0; i < len(body) && len(handles) < MaxGenerationMentions; i++ {
		if body[i] != '@' {
			continue
		}
		if i > 0 {
			previous, _ := utf8.DecodeLastRuneInString(body[:i])
			if generationEmbeddedRune(previous) || strings.ContainsRune(".%+-", previous) {
				continue
			}
		}
		end := i + 1
		for end < len(body) && isTagCharacter(body[end]) {
			end++
		}
		if end < len(body) {
			next, _ := utf8.DecodeRuneInString(body[end:])
			if generationEmbeddedRune(next) || next == '-' || next == '+' || next == '%' {
				continue
			}
			// A sentence-ending period is fine; a domain/member suffix is not.
			if next == '.' && end+1 < len(body) {
				afterDot, _ := utf8.DecodeRuneInString(body[end+1:])
				if generationEmbeddedRune(afterDot) {
					continue
				}
			}
		}
		handle, err := NormalizeHandle(body[i+1 : end])
		if err == nil && !seen[handle] {
			handles = append(handles, handle)
			seen[handle] = true
		}
		i = end - 1
	}
	return handles
}

func generationEmbeddedRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || strings.ContainsRune("_@/\\", r)
}

func ScheduledGenerationKey(slot time.Time) (string, error) {
	if slot.IsZero() {
		return "", fmt.Errorf("scheduled key requires a slot")
	}
	return "scheduled:v1:" + GenerationInstant(slot).Format(time.RFC3339Nano), nil
}

// SocialGenerationKey identifies the committed action, not an enqueue attempt.
// For quotes the action/source conversation is the newly created quote post;
// direct social generation produces a flat reply, not a nested reply or quote.
func SocialGenerationKey(kind GenerationTrigger, actionID ID) (string, error) {
	if !socialGenerationTrigger(kind) || !validGenerationID(actionID) {
		return "", fmt.Errorf("invalid social trigger identity")
	}
	return string(kind) + ":v1:" + string(actionID), nil
}

// GenerationCooldownKey deliberately has no transient reply/repost/action ID.
// conversationID is the canonical target post (the new post for quote actions).
func GenerationCooldownKey(actorID, agentID, conversationID ID, kind GenerationTrigger) (string, error) {
	if !validGenerationID(actorID) || !validGenerationID(agentID) || !validGenerationID(conversationID) || actorID == agentID || !socialGenerationTrigger(kind) {
		return "", fmt.Errorf("invalid cooldown identity")
	}
	return "cooldown:v1:" + string(actorID) + ":" + string(agentID) + ":" + string(conversationID) + ":" + string(kind), nil
}

func socialGenerationTrigger(kind GenerationTrigger) bool {
	switch kind {
	case TriggerReply, TriggerRepost, TriggerQuote, TriggerHumanPost, TriggerContinuation:
		return true
	}
	return false
}

// GenerationResponseTiming uses original source time, never active hours or a
// retry's creation time. A late but fresh admission can be immediately available;
// an expired source cannot be revived. Persist the returned values once.
func GenerationResponseTiming(policy GenerationPolicy, sourceAt, now time.Time, draw func(int64) int64) (time.Time, time.Time, error) {
	if err := policy.Validate(); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if sourceAt.IsZero() || now.IsZero() || sourceAt.After(now) {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid source time")
	}
	sourceAt, now = GenerationInstant(sourceAt), GenerationInstant(now)
	expiresAt := sourceAt.Add(time.Duration(policy.SourceMaxAgeSeconds) * time.Second)
	if !now.Before(expiresAt) {
		return time.Time{}, time.Time{}, fmt.Errorf("expired generation source")
	}
	delay, err := generationDraw(draw, int64(policy.ResponseMaxDelaySeconds-policy.ResponseMinDelaySeconds+1))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	availableAt := laterGenerationTime(now, sourceAt.Add(time.Duration(int64(policy.ResponseMinDelaySeconds)+delay)*time.Second))
	return availableAt, expiresAt, nil
}
