// Package diff computes deterministic, side-effect-free sync previews. It does
// not grant target write permission and cannot establish creation evidence.
package diff

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
)

type Options struct {
	Now             time.Time
	TitlePolicy     domain.TitlePolicy
	BindingVersion  uint64
	StableSourceIDs bool
}

func Preview(previous *domain.SourceSnapshot, current domain.SourceSnapshot, baseline, target *domain.TargetSnapshot, options Options) (domain.SyncPlan, error) {
	currentHash, err := current.ComputeHash()
	if err != nil {
		return domain.SyncPlan{}, err
	}
	if current.BusinessHash != "" && current.BusinessHash != currentHash {
		return domain.SyncPlan{}, errors.New("current source business_hash does not match its content")
	}
	current.BusinessHash = currentHash
	if (baseline == nil) != (target == nil) {
		return domain.SyncPlan{}, errors.New("target baseline and current target must be supplied together")
	}
	if baseline != nil && (!baseline.OfflineSimulation || !target.OfflineSimulation) {
		return domain.SyncPlan{}, errors.New("M1 target inputs must explicitly set offline_simulation=true")
	}
	if options.TitlePolicy == "" {
		options.TitlePolicy = domain.TitleFollowSource
	}
	if options.TitlePolicy != domain.TitleFollowSource && options.TitlePolicy != domain.TitleTargetOverride {
		return domain.SyncPlan{}, errors.New("unknown title policy")
	}
	if err := validateSource(current); err != nil {
		return domain.SyncPlan{}, err
	}
	if previous != nil {
		if err := validateSource(*previous); err != nil {
			return domain.SyncPlan{}, fmt.Errorf("invalid previous source snapshot: %w", err)
		}
		previousHash, err := previous.ComputeHash()
		if err != nil {
			return domain.SyncPlan{}, err
		}
		if previous.BusinessHash != "" && previous.BusinessHash != previousHash {
			return domain.SyncPlan{}, errors.New("previous source business_hash does not match its content")
		}
		previous.BusinessHash = previousHash
	}
	if baseline != nil {
		if err := validateTarget(*baseline); err != nil {
			return domain.SyncPlan{}, fmt.Errorf("invalid target baseline: %w", err)
		}
		if err := validateTarget(*target); err != nil {
			return domain.SyncPlan{}, fmt.Errorf("invalid current target: %w", err)
		}
	}

	now := options.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	plan := domain.SyncPlan{
		ReadOnly: true, SourceSnapshotHash: current.BusinessHash,
		BindingVersion: options.BindingVersion, ParserVersion: current.ParserVersion,
		PolicyVersion: domain.PolicyVersion, AllowedActions: []string{}, ExpiresAt: now.Add(15 * time.Minute),
	}
	if baseline != nil {
		hash, err := baseline.ComputeHash()
		if err != nil {
			return domain.SyncPlan{}, err
		}
		plan.TargetBaselineHash = hash
	}

	if previous == nil {
		for _, message := range current.Messages {
			plan.Diff.Added = append(plan.Diff.Added, domain.MessageChange{SourceID: message.ID, Kind: "add"})
		}
		plan.Diff.TitleChange = "set_source"
		plan.Diff.SourceTitle = current.Title
		plan.Status = "needs_binding_confirmation"
		plan.NextAction = "review the source range; production binding confirmation is not implemented in M1"
		if current.Coverage.Status == "partial" {
			plan.Status = "unsupported_content"
			plan.NextAction = "review unsupported source items before any future binding"
		}
		plan.ID = planID(plan, target)
		return plan, nil
	}

	plan.Diff.PreviousTitle = previous.Title
	plan.Diff.SourceTitle = current.Title
	previousByID := indexSource(previous.Messages)
	currentByID := indexSource(current.Messages)
	if previous.Identity.CandidateID != "" && current.Identity.CandidateID != "" &&
		previous.Identity.CandidateID != current.Identity.CandidateID {
		plan.Diff.TargetConflicts = append(plan.Diff.TargetConflicts, domain.MessageChange{
			Kind: "source_identity_changed", Detail: "candidate source identity changed; do not reuse the existing target",
		})
	}
	if previous.ParserVersion != current.ParserVersion {
		plan.Diff.TargetConflicts = append(plan.Diff.TargetConflicts, domain.MessageChange{
			Kind: "parser_version_changed", Detail: "parser policy changed; re-review coverage before applying",
		})
	}
	for _, oldMessage := range previous.Messages {
		newMessage, present := currentByID[oldMessage.ID]
		if !present {
			plan.Diff.ScopeReduced = append(plan.Diff.ScopeReduced, domain.MessageChange{
				SourceID: oldMessage.ID, Kind: "source_message_missing", Detail: "absence is not treated as deletion",
			})
			continue
		}
		if oldMessage.Order != newMessage.Order || oldMessage.ParentNodeID != newMessage.ParentNodeID {
			plan.Diff.TargetConflicts = append(plan.Diff.TargetConflicts, domain.MessageChange{
				SourceID: oldMessage.ID, Kind: "source_structure_changed", Detail: "branch or order changes require explicit handling",
			})
			continue
		}
		if !messageContentEqual(oldMessage, newMessage) {
			plan.Diff.Modified = append(plan.Diff.Modified, domain.MessageChange{SourceID: oldMessage.ID, Kind: "modify"})
		}
	}
	for _, newMessage := range current.Messages {
		if _, present := previousByID[newMessage.ID]; !present {
			plan.Diff.Added = append(plan.Diff.Added, domain.MessageChange{SourceID: newMessage.ID, Kind: "add"})
		}
	}
	if !sameRelativeOrder(previous.Messages, current.Messages) {
		plan.Diff.TargetConflicts = append(plan.Diff.TargetConflicts, domain.MessageChange{
			Kind: "source_order_changed", Detail: "existing source message order changed",
		})
	}

	if !options.StableSourceIDs && previous.BusinessHash != current.BusinessHash {
		plan.Diff.TargetConflicts = append(plan.Diff.TargetConflicts, domain.MessageChange{
			Kind: "source_identity_unverified", Detail: "automatic ID matching is disabled unless the offline fixture explicitly declares stable source IDs",
		})
	}
	if baseline != nil {
		compareTarget(previousByID, currentByID, *baseline, *target, &plan.Diff)
		plan.Diff.TargetTitle = target.Title
	}
	plan.Diff.TitleChange = titleChange(previous.Title, current.Title, baseline, target, options.TitlePolicy)

	conflicted := len(plan.Diff.ScopeReduced) > 0 || len(plan.Diff.TargetConflicts) > 0
	changed := len(plan.Diff.Added) > 0 || len(plan.Diff.Modified) > 0 || plan.Diff.TitleChange == "sync_source"
	switch {
	case current.Coverage.Status == "partial":
		plan.Status = "unsupported_content"
		plan.NextAction = "review unsupported source items; this preview cannot be applied"
	case conflicted:
		plan.Status = "conflict"
		plan.NextAction = "resolve the reported range or target conflict; no target write is allowed"
	case !changed:
		plan.Status = "no_change"
		plan.NextAction = "none; if ChatGPT changed, update the share snapshot first"
	default:
		plan.Status = "ready"
		plan.NextAction = "read-only local preview only; production apply is not implemented"
	}
	plan.ID = planID(plan, target)
	return plan, nil
}

func validateSource(snapshot domain.SourceSnapshot) error {
	seen := make(map[string]struct{}, len(snapshot.Messages))
	for _, message := range snapshot.Messages {
		if message.ID == "" {
			return errors.New("source message id is empty")
		}
		if _, duplicate := seen[message.ID]; duplicate {
			return fmt.Errorf("duplicate source message id %q", message.ID)
		}
		seen[message.ID] = struct{}{}
	}
	return nil
}

func validateTarget(snapshot domain.TargetSnapshot) error {
	seenIDs := make(map[string]struct{}, len(snapshot.Messages))
	seenSources := make(map[string]struct{}, len(snapshot.Messages))
	lastOrder := -1
	for index, message := range snapshot.Messages {
		if message.ID == "" {
			return errors.New("target message id is empty")
		}
		if _, duplicate := seenIDs[message.ID]; duplicate {
			return fmt.Errorf("duplicate target message id %q", message.ID)
		}
		seenIDs[message.ID] = struct{}{}
		if message.Order < 0 {
			return fmt.Errorf("target message %q has a negative order", message.ID)
		}
		if index > 0 && message.Order <= lastOrder {
			return errors.New("target messages are not in strictly increasing order")
		}
		lastOrder = message.Order
		if message.Managed {
			if message.SourceID == "" {
				return errors.New("managed target message has no source id")
			}
			if _, duplicate := seenSources[message.SourceID]; duplicate {
				return fmt.Errorf("duplicate managed source id %q", message.SourceID)
			}
			seenSources[message.SourceID] = struct{}{}
		}
	}
	if snapshot.CurrentMessageID != "" {
		if _, present := seenIDs[snapshot.CurrentMessageID]; !present {
			return fmt.Errorf("current target message id %q does not exist", snapshot.CurrentMessageID)
		}
	}
	return nil
}

func compareTarget(previous, current map[string]domain.Message, baseline, target domain.TargetSnapshot, result *domain.Diff) {
	if baseline.CurrentMessageID != target.CurrentMessageID {
		result.TargetConflicts = append(result.TargetConflicts, domain.MessageChange{
			Kind: "target_current_message_changed", Detail: "current target node differs from the verified baseline",
		})
	}
	baselineByID := indexTargetID(baseline.Messages)
	targetByID := indexTargetID(target.Messages)
	for _, baseMessage := range baseline.Messages {
		currentTarget, present := targetByID[baseMessage.ID]
		if !present {
			result.TargetConflicts = append(result.TargetConflicts, domain.MessageChange{
				SourceID: baseMessage.SourceID, Kind: "target_message_missing", Detail: "target removal is never overwritten automatically",
			})
			continue
		}
		if !targetMessageStructureEqual(baseMessage, currentTarget) {
			result.TargetConflicts = append(result.TargetConflicts, domain.MessageChange{
				SourceID: baseMessage.SourceID, Kind: "target_structure_changed", Detail: "target message structure differs from the verified baseline",
			})
			continue
		}
		if targetMessageContentEqual(baseMessage, currentTarget) {
			continue
		}
		newSource, sourcePresent := current[baseMessage.SourceID]
		oldSource, oldPresent := previous[baseMessage.SourceID]
		if sourcePresent && oldPresent && !messageContentEqual(oldSource, newSource) && targetMatchesSource(currentTarget, newSource) {
			continue
		}
		result.TargetConflicts = append(result.TargetConflicts, domain.MessageChange{
			SourceID: baseMessage.SourceID, Kind: "target_message_changed", Detail: "current target differs from the verified baseline",
		})
	}
	for _, currentTarget := range target.Messages {
		if _, present := baselineByID[currentTarget.ID]; present {
			continue
		}
		kind := "target_native_message"
		if currentTarget.Managed {
			kind = "unexpected_managed_target_message"
		}
		result.TargetConflicts = append(result.TargetConflicts, domain.MessageChange{
			SourceID: currentTarget.SourceID, Kind: kind, Detail: "target structure diverged from the verified baseline",
		})
	}
}

func titleChange(previous, current string, baseline, target *domain.TargetSnapshot, policy domain.TitlePolicy) string {
	if policy == domain.TitleTargetOverride {
		return "preserve_target_override"
	}
	if baseline != nil && target.Title != baseline.Title {
		if target.Title == current {
			return "converged"
		}
		return "preserve_target_override"
	}
	if previous != current {
		return "sync_source"
	}
	return "unchanged"
}

func indexSource(messages []domain.Message) map[string]domain.Message {
	result := make(map[string]domain.Message, len(messages))
	for _, message := range messages {
		result[message.ID] = message
	}
	return result
}

func indexTargetID(messages []domain.TargetMessage) map[string]domain.TargetMessage {
	result := make(map[string]domain.TargetMessage, len(messages))
	for _, message := range messages {
		result[message.ID] = message
	}
	return result
}

func messageContentEqual(left, right domain.Message) bool {
	return left.Role == right.Role && left.Channel == right.Channel && slices.Equal(left.Parts, right.Parts)
}

func targetMessageStructureEqual(left, right domain.TargetMessage) bool {
	return left.SourceID == right.SourceID && left.Managed == right.Managed && left.Order == right.Order
}

func targetMessageContentEqual(left, right domain.TargetMessage) bool {
	return left.Role == right.Role && left.Channel == right.Channel && slices.Equal(left.Parts, right.Parts)
}

func targetMatchesSource(target domain.TargetMessage, source domain.Message) bool {
	return target.Managed && target.SourceID == source.ID && target.Role == source.Role &&
		target.Channel == source.Channel && slices.Equal(target.Parts, source.Parts)
}

func sameRelativeOrder(previous, current []domain.Message) bool {
	positions := make(map[string]int, len(current))
	for index, message := range current {
		positions[message.ID] = index
	}
	last := -1
	for _, message := range previous {
		position, present := positions[message.ID]
		if !present {
			continue
		}
		if position <= last {
			return false
		}
		last = position
	}
	return true
}

func planID(plan domain.SyncPlan, target *domain.TargetSnapshot) string {
	payload := plan.SourceSnapshotHash + "\x00" + plan.TargetBaselineHash + "\x00" + plan.PolicyVersion
	if target != nil {
		hash, _ := target.ComputeHash()
		payload += "\x00" + hash
	}
	sum := sha256.Sum256([]byte(payload))
	return "preview_" + hex.EncodeToString(sum[:8])
}
