package diff

import (
	"testing"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
)

var fixedNow = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

func TestFirstPreviewNeedsConfirmationAndCannotApply(t *testing.T) {
	current := source("Title", message("u1", "user", "same"), message("a1", "assistant", "same"))
	plan, err := Preview(nil, current, nil, nil, Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "needs_binding_confirmation" || !plan.ReadOnly || len(plan.AllowedActions) != 0 {
		t.Fatalf("unsafe first plan: %#v", plan)
	}
	if len(plan.Diff.Added) != 2 {
		t.Fatalf("same text with different IDs must remain two messages: %#v", plan.Diff)
	}
}

func TestAppendNoChangeAndModification(t *testing.T) {
	previous := source("Title", message("u1", "user", "question"), message("a1", "assistant", "answer"))
	tests := []struct {
		name     string
		current  domain.SourceSnapshot
		status   string
		added    int
		modified int
	}{
		{"no change", previous, "no_change", 0, 0},
		{"append", source("Title", message("u1", "user", "question"), message("a1", "assistant", "answer"), message("u2", "user", "more")), "ready", 1, 0},
		{"modify", source("Title", message("u1", "user", "changed"), message("a1", "assistant", "answer")), "ready", 0, 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := Preview(&previous, test.current, nil, nil, Options{Now: fixedNow, StableSourceIDs: true})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Status != test.status || len(plan.Diff.Added) != test.added || len(plan.Diff.Modified) != test.modified {
				t.Fatalf("unexpected plan: %#v", plan)
			}
		})
	}
}

func TestChangedSnapshotRequiresExplicitStableIDDeclaration(t *testing.T) {
	previous := source("Title", message("u1", "user", "question"))
	current := source("Title", message("u1", "user", "changed"))
	plan, err := Preview(&previous, current, nil, nil, Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "conflict" || !hasConflict(plan.Diff, "source_identity_unverified") {
		t.Fatalf("unverified IDs should block automatic merge: %#v", plan)
	}
}

func TestCandidateSourceIdentityChangeConflicts(t *testing.T) {
	previous := source("Title", message("u1", "user", "question"))
	current := source("Title", message("u1", "user", "question"), message("a1", "assistant", "answer"))
	previous.Identity = domain.SourceIdentity{CandidateID: "source-a", Evidence: domain.EvidenceCandidate}
	current.Identity = domain.SourceIdentity{CandidateID: "source-b", Evidence: domain.EvidenceCandidate}
	previous.BusinessHash, _ = previous.ComputeHash()
	current.BusinessHash, _ = current.ComputeHash()
	plan, err := Preview(&previous, current, nil, nil, Options{Now: fixedNow, StableSourceIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "conflict" || !hasConflict(plan.Diff, "source_identity_changed") {
		t.Fatalf("source identity change should block target reuse: %#v", plan)
	}
}

func TestScopeReductionDoesNotDelete(t *testing.T) {
	previous := source("Title", message("u1", "user", "question"), message("a1", "assistant", "answer"))
	current := source("Title", message("u1", "user", "question"))
	plan, err := Preview(&previous, current, nil, nil, Options{Now: fixedNow, StableSourceIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "conflict" || len(plan.Diff.ScopeReduced) != 1 || plan.Diff.ScopeReduced[0].Kind != "source_message_missing" {
		t.Fatalf("scope reduction was not blocked: %#v", plan)
	}
}

func TestTargetContentConflict(t *testing.T) {
	previous := source("Title", message("u1", "user", "question"))
	current := source("Title", message("u1", "user", "source changed"))
	baseline := target("Title", targetMessage("t1", "u1", "user", "question"))
	actual := target("Title", targetMessage("t1", "u1", "user", "target changed"))
	plan, err := Preview(&previous, current, &baseline, &actual, Options{Now: fixedNow, StableSourceIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "conflict" || !hasConflict(plan.Diff, "target_message_changed") {
		t.Fatalf("divergent target edit was not blocked: %#v", plan)
	}
}

func TestTargetNativeContinuationConflicts(t *testing.T) {
	previous := source("Title", message("u1", "user", "question"))
	current := source("Title", message("u1", "user", "question"), message("a1", "assistant", "answer"))
	baseline := target("Title", targetMessage("t1", "u1", "user", "question"))
	actual := target("Title", targetMessage("t1", "u1", "user", "question"))
	actual.Messages = append(actual.Messages, domain.TargetMessage{ID: "native", Role: "user", Parts: []string{"OWU continuation"}, Order: 1})
	plan, err := Preview(&previous, current, &baseline, &actual, Options{Now: fixedNow, StableSourceIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "conflict" || !hasConflict(plan.Diff, "target_native_message") {
		t.Fatalf("native continuation was not protected: %#v", plan)
	}
}

func TestTargetCurrentMessageValidationAndConflict(t *testing.T) {
	previous := source("Old", message("u1", "user", "question"), message("a1", "assistant", "answer"))
	current := source("New", message("u1", "user", "question"), message("a1", "assistant", "answer"))
	baseline := target("Old",
		targetMessage("t1", "u1", "user", "question"),
		targetMessage("t2", "a1", "assistant", "answer"),
	)
	baseline.CurrentMessageID = "t2"

	t.Run("valid cursor switch conflicts", func(t *testing.T) {
		actual := baseline
		actual.CurrentMessageID = "t1"
		plan, err := Preview(&previous, current, &baseline, &actual, Options{Now: fixedNow, StableSourceIDs: true})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Status != "conflict" || !hasConflict(plan.Diff, "target_current_message_changed") {
			t.Fatalf("target cursor change was not blocked: %#v", plan)
		}
	})

	t.Run("missing cursor target is invalid", func(t *testing.T) {
		actual := baseline
		actual.CurrentMessageID = "missing"
		if _, err := Preview(&previous, current, &baseline, &actual, Options{Now: fixedNow, StableSourceIDs: true}); err == nil {
			t.Fatal("non-empty missing current target id should be rejected")
		}
	})

	t.Run("legacy omitted cursors remain valid", func(t *testing.T) {
		legacyBaseline := baseline
		legacyBaseline.CurrentMessageID = ""
		legacyTarget := legacyBaseline
		plan, err := Preview(&previous, current, &legacyBaseline, &legacyTarget, Options{Now: fixedNow, StableSourceIDs: true})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Status != "ready" {
			t.Fatalf("omitted optional cursors should remain usable: %#v", plan)
		}
	})
}

func TestTargetConvergenceDoesNotHideStructureChanges(t *testing.T) {
	previous := source("Title", message("u1", "user", "question"), message("a1", "assistant", "old answer"))
	current := source("Title", message("u1", "user", "question"), message("a1", "assistant", "converged answer"))
	baseline := target("Title",
		targetMessage("t1", "u1", "user", "question"),
		targetMessage("t2", "a1", "assistant", "old answer"),
	)

	t.Run("content-only convergence is accepted", func(t *testing.T) {
		actual := cloneTarget(baseline)
		actual.Messages[1].Parts = []string{"converged answer"}
		plan, err := Preview(&previous, current, &baseline, &actual, Options{Now: fixedNow, StableSourceIDs: true})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Status == "conflict" {
			t.Fatalf("content convergence should not conflict: %#v", plan)
		}
	})

	t.Run("converged content with changed order conflicts", func(t *testing.T) {
		actual := cloneTarget(baseline)
		actual.Messages[1].Parts = []string{"converged answer"}
		actual.Messages[1].Order = 99
		plan, err := Preview(&previous, current, &baseline, &actual, Options{Now: fixedNow, StableSourceIDs: true})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Status != "conflict" || !hasConflict(plan.Diff, "target_structure_changed") {
			t.Fatalf("content convergence hid target reordering: %#v", plan)
		}
	})
}

func TestTitleChangeAndOverride(t *testing.T) {
	previous := source("Old", message("u1", "user", "question"))
	current := source("New", message("u1", "user", "question"))
	baseline := target("Old", targetMessage("t1", "u1", "user", "question"))
	unchangedTarget := target("Old", targetMessage("t1", "u1", "user", "question"))
	plan, err := Preview(&previous, current, &baseline, &unchangedTarget, Options{Now: fixedNow, StableSourceIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "ready" || plan.Diff.TitleChange != "sync_source" {
		t.Fatalf("source title change not planned: %#v", plan)
	}
	customTarget := target("Custom", targetMessage("t1", "u1", "user", "question"))
	plan, err = Preview(&previous, current, &baseline, &customTarget, Options{Now: fixedNow, StableSourceIDs: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Diff.TitleChange != "preserve_target_override" || plan.Status != "no_change" {
		t.Fatalf("custom target title should be preserved: %#v", plan)
	}
	plan, err = Preview(&previous, current, &baseline, &unchangedTarget, Options{
		Now: fixedNow, StableSourceIDs: true, TitlePolicy: domain.TitleTargetOverride,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Diff.TitleChange != "preserve_target_override" {
		t.Fatalf("explicit title override lost: %#v", plan)
	}
}

func TestOfflineTargetMustBeExplicit(t *testing.T) {
	previous := source("Title", message("u1", "user", "question"))
	baseline := target("Title", targetMessage("t1", "u1", "user", "question"))
	actual := baseline
	actual.OfflineSimulation = false
	if _, err := Preview(&previous, previous, &baseline, &actual, Options{Now: fixedNow}); err == nil {
		t.Fatal("unmarked target input should be rejected")
	}
}

func TestRejectsTamperedBusinessHash(t *testing.T) {
	current := source("Title", message("u1", "user", "question"))
	current.BusinessHash = "tampered"
	if _, err := Preview(nil, current, nil, nil, Options{Now: fixedNow}); err == nil {
		t.Fatal("tampered source hash should be rejected")
	}
}

func source(title string, messages ...domain.Message) domain.SourceSnapshot {
	for index := range messages {
		messages[index].Order = index
		if index > 0 {
			messages[index].ParentNodeID = messages[index-1].NodeID
		}
	}
	snapshot := domain.SourceSnapshot{
		SourceType: domain.SourceChatGPTShare, Title: title, Messages: messages,
		ParserVersion: domain.ParserVersion, Coverage: domain.Coverage{Status: "supported_path_complete"},
	}
	hash, err := snapshot.ComputeHash()
	if err != nil {
		panic(err)
	}
	snapshot.BusinessHash = hash
	return snapshot
}

func message(id, role, text string) domain.Message {
	return domain.Message{ID: id, NodeID: "node-" + id, Role: role, Parts: []string{text}}
}

func target(title string, messages ...domain.TargetMessage) domain.TargetSnapshot {
	for index := range messages {
		messages[index].Order = index
	}
	return domain.TargetSnapshot{OfflineSimulation: true, Title: title, Messages: messages}
}

func targetMessage(id, sourceID, role, text string) domain.TargetMessage {
	return domain.TargetMessage{ID: id, SourceID: sourceID, Role: role, Parts: []string{text}, Managed: true}
}

func cloneTarget(snapshot domain.TargetSnapshot) domain.TargetSnapshot {
	clone := snapshot
	clone.Messages = append([]domain.TargetMessage(nil), snapshot.Messages...)
	for index := range clone.Messages {
		clone.Messages[index].Parts = append([]string(nil), snapshot.Messages[index].Parts...)
	}
	return clone
}

func hasConflict(result domain.Diff, kind string) bool {
	for _, conflict := range result.TargetConflicts {
		if conflict.Kind == kind {
			return true
		}
	}
	return false
}
