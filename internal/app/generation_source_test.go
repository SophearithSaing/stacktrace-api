package app

import (
	"testing"
	"time"
)

func TestGenerationSourceInvalidation(t *testing.T) {
	before := testRunningJob()
	actor, post := NewID(), NewID()
	before.TriggerKind, before.OutputKind, before.TriggerActorID, before.SourcePostID, before.CooldownKey = TriggerHumanPost, OutputReply, &actor, &post, "cooldown"
	now := before.CreatedAt.Add(2 * time.Minute) // Owner lease is already expired.
	after := before
	after.Status, after.ReasonCode, after.FinishedAt, after.LeaseExpiresAt = JobCancelled, "source_removed", &now, nil
	if err := ValidateGenerationSourceInvalidation(before, after, before.LeaseVersion, now); err != nil {
		t.Fatal(err)
	}
	if ValidateGenerationJobTransition(before, after, before.LeaseVersion, now, nil) == nil {
		t.Fatal("ordinary expired owner gained cancellation authority")
	}
	for _, change := range []func(*GenerationJob){
		func(j *GenerationJob) { j.LeaseVersion++ }, func(j *GenerationJob) { j.ExpiresAt = j.ExpiresAt.Add(time.Hour) }, func(j *GenerationJob) { j.ReasonCode = "other" }, func(j *GenerationJob) { j.AvailableAt = j.AvailableAt.Add(time.Second) },
	} {
		changed := after
		change(&changed)
		if ValidateGenerationSourceInvalidation(before, changed, before.LeaseVersion, now) == nil {
			t.Fatal("accepted invalid cleanup")
		}
	}
	if ValidateGenerationSourceInvalidation(before, after, before.LeaseVersion-1, now) == nil {
		t.Fatal("accepted stale fence")
	}
	now = before.ExpiresAt
	after.Status, after.ReasonCode, after.FinishedAt = JobSkipped, "stale_trigger", &now
	if err := ValidateGenerationSourceInvalidation(before, after, before.LeaseVersion, now); err != nil {
		t.Fatal(err)
	}
	if ValidateGenerationSourceInvalidation(after, after, after.LeaseVersion, now) == nil {
		t.Fatal("mutated terminal source job")
	}
}
