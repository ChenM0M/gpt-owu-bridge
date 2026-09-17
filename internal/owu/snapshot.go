package owu

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
)

const (
	bridgeMetadataKey = "gpt_owu_gate"
	bridgeSchema      = "1"
)

// Snapshot retains both a transport-independent projection and the complete
// chat document returned by Open WebUI. ConflictHash covers the complete
// relevant response, including unknown chat/message metadata, so a future OWU
// field or a native branch cannot disappear behind the normalized projection.
type Snapshot struct {
	ChatID       string                `json:"chat_id,omitempty"`
	OwnerID      string                `json:"owner_id,omitempty"`
	Archived     bool                  `json:"archived"`
	Pinned       bool                  `json:"pinned"`
	FolderID     string                `json:"folder_id,omitempty"`
	UpdatedAt    int64                 `json:"updated_at,omitempty"`
	Normalized   domain.TargetSnapshot `json:"normalized"`
	RawChat      json.RawMessage       `json:"raw_chat"`
	RawEnvelope  json.RawMessage       `json:"raw_envelope,omitempty"`
	ConflictHash string                `json:"conflict_hash"`
}

func (s Snapshot) ToTargetSnapshot() domain.TargetSnapshot { return s.Normalized }

// Equivalent is intentionally stricter than normalized equality. It is used
// for a write-before-read guard and detects native metadata and graph changes.
func Equivalent(left, right Snapshot) bool {
	return left.ChatID == right.ChatID && left.OwnerID == right.OwnerID &&
		left.ConflictHash != "" && left.ConflictHash == right.ConflictHash
}

// MatchTarget compares the normalized fields for callers that only have a
// persisted domain projection. Post-write verification should use
// MatchSnapshot, which also verifies parents, children and unknown metadata.
func MatchTarget(actual Snapshot, expected domain.TargetSnapshot) bool {
	return equalNormalized(actual.Normalized, expected)
}

// MatchSnapshot performs strict semantic post-write verification. Only the
// server-owned chat id and the operation receipt marker are ignored; the full
// history graph, active branch view, managed content and unknown metadata must
// otherwise match.
func MatchSnapshot(actual, expected Snapshot) bool {
	if (expected.ChatID != "" && actual.ChatID != expected.ChatID) ||
		(expected.OwnerID != "" && actual.OwnerID != expected.OwnerID) ||
		actual.Archived != expected.Archived || actual.Pinned != expected.Pinned ||
		actual.FolderID != expected.FolderID {
		return false
	}
	actualChat, expectedChat, chatErr := comparableChats(actual.RawChat, expected.RawChat)
	if chatErr != nil || !bytes.Equal(actualChat, expectedChat) {
		return false
	}
	if len(expected.RawEnvelope) == 0 {
		return true
	}
	actualEnvelope, actualErr := comparableEnvelope(actual.RawEnvelope)
	expectedEnvelope, expectedErr := comparableEnvelope(expected.RawEnvelope)
	return actualErr == nil && expectedErr == nil && bytes.Equal(actualEnvelope, expectedEnvelope)
}

// BuildSnapshot deterministically converts a source snapshot into a complete
// Open WebUI chat document. When current is supplied, unknown chat fields and
// per-message metadata are retained. Existing messages are never deleted.
func BuildSnapshot(source domain.SourceSnapshot, current *Snapshot) (Snapshot, error) {
	if len(source.Messages) == 0 {
		return Snapshot{}, errors.New("source snapshot has no messages")
	}
	if source.Identity.CandidateID == "" && source.ShareID == "" {
		return Snapshot{}, errors.New("source snapshot has no stable identity basis")
	}
	title := source.Title
	if title == "" {
		title = "Imported ChatGPT conversation"
	}
	ordered := append([]domain.Message(nil), source.Messages...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })
	seenSources := make(map[string]struct{}, len(ordered))
	seenNodes := make(map[string]struct{}, len(ordered))
	for index, message := range ordered {
		if message.ID == "" || message.NodeID == "" || message.Role == "" || len(message.Parts) == 0 {
			return Snapshot{}, errors.New("source snapshot contains an incomplete message")
		}
		if _, exists := seenSources[message.ID]; exists {
			return Snapshot{}, errors.New("source snapshot contains duplicate message ids")
		}
		if _, exists := seenNodes[message.NodeID]; exists {
			return Snapshot{}, errors.New("source snapshot contains duplicate node ids")
		}
		seenSources[message.ID] = struct{}{}
		seenNodes[message.NodeID] = struct{}{}
		if index > 0 && ordered[index-1].Order == message.Order {
			return Snapshot{}, errors.New("source snapshot contains duplicate message order")
		}
		if message.Role != "user" && message.Role != "assistant" {
			return Snapshot{}, errors.New("source snapshot contains an unsupported role")
		}
	}

	chat := map[string]json.RawMessage{}
	var result Snapshot
	if current != nil {
		if err := decodeSingleJSON(current.RawChat, &chat); err != nil {
			return Snapshot{}, errors.New("current target raw chat is invalid")
		}
		result.ChatID = current.ChatID
		result.OwnerID = current.OwnerID
		result.Archived = current.Archived
		result.Pinned = current.Pinned
		result.FolderID = current.FolderID
		result.UpdatedAt = current.UpdatedAt
		result.RawEnvelope = cloneRaw(current.RawEnvelope)
	}

	history := map[string]json.RawMessage{}
	messages := map[string]json.RawMessage{}
	if rawHistory, ok := chat["history"]; ok {
		if err := json.Unmarshal(rawHistory, &history); err != nil {
			return Snapshot{}, errors.New("current target history is invalid")
		}
		if rawMessages, ok := history["messages"]; ok {
			if err := json.Unmarshal(rawMessages, &messages); err != nil {
				return Snapshot{}, errors.New("current target messages are invalid")
			}
		}
	}

	existingBySource := make(map[string]string)
	for targetID, raw := range messages {
		var message map[string]json.RawMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			return Snapshot{}, errors.New("current target contains an invalid message")
		}
		metadata := readBridgeMetadata(message)
		if metadata.SourceMessageID != "" {
			if _, duplicate := existingBySource[metadata.SourceMessageID]; duplicate {
				return Snapshot{}, errors.New("current target contains duplicate managed source ids")
			}
			existingBySource[metadata.SourceMessageID] = targetID
		}
	}

	targetIDs := make([]string, len(ordered))
	for index, message := range ordered {
		if existing := existingBySource[message.ID]; existing != "" {
			targetIDs[index] = existing
		} else {
			targetIDs[index] = deterministicMessageID(source, message.ID)
		}
	}
	if duplicateString(targetIDs) {
		return Snapshot{}, errors.New("target message id collision")
	}

	managedIDs := make(map[string]struct{}, len(targetIDs))
	for _, id := range targetIDs {
		managedIDs[id] = struct{}{}
	}
	for index, sourceMessage := range ordered {
		targetID := targetIDs[index]
		message := map[string]json.RawMessage{}
		if raw, ok := messages[targetID]; ok {
			if err := json.Unmarshal(raw, &message); err != nil {
				return Snapshot{}, errors.New("current target managed message is invalid")
			}
		}
		parentID := ""
		if index > 0 {
			parentID = targetIDs[index-1]
		}
		children := preservedUnmanagedChildren(message, managedIDs)
		if index+1 < len(targetIDs) {
			children = append([]string{targetIDs[index+1]}, children...)
		}

		setJSON(message, "id", targetID)
		if parentID == "" {
			message["parentId"] = json.RawMessage("null")
		} else {
			setJSON(message, "parentId", parentID)
		}
		setJSON(message, "childrenIds", children)
		setJSON(message, "role", sourceMessage.Role)
		setJSON(message, "content", strings.Join(sourceMessage.Parts, ""))
		if sourceMessage.SourceCreatedAt != nil {
			setJSON(message, "timestamp", sourceMessage.SourceCreatedAt.Unix())
		}
		if sourceMessage.Role == "assistant" {
			setJSON(message, "done", true)
		}
		setJSON(message, bridgeMetadataKey, bridgeMessageMetadata{
			Schema: bridgeSchema, SourceMessageID: sourceMessage.ID,
			SourceNodeID: sourceMessage.NodeID, Channel: sourceMessage.Channel,
			Parts: append([]string(nil), sourceMessage.Parts...),
		})
		encoded, err := json.Marshal(message)
		if err != nil {
			return Snapshot{}, fmt.Errorf("encode target message: %w", err)
		}
		messages[targetID] = encoded
	}

	setJSON(history, "messages", messages)
	setJSON(history, "currentId", targetIDs[len(targetIDs)-1])
	setJSON(chat, "title", title)
	setJSON(chat, "history", history)

	active := make([]json.RawMessage, 0, len(targetIDs))
	for _, id := range targetIDs {
		active = append(active, messages[id])
	}
	setJSON(chat, "messages", active)

	rawChat, err := json.Marshal(chat)
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode target chat: %w", err)
	}
	result.RawChat = rawChat
	normalized, err := normalizeChat(rawChat)
	if err != nil {
		return Snapshot{}, err
	}
	result.Normalized = normalized
	result.ConflictHash, err = hashRelevant(result)
	if err != nil {
		return Snapshot{}, err
	}
	result.Normalized.BusinessHash = result.ConflictHash
	return result, nil
}

type bridgeMessageMetadata struct {
	Schema          string   `json:"schema"`
	SourceMessageID string   `json:"source_message_id"`
	SourceNodeID    string   `json:"source_node_id,omitempty"`
	Channel         string   `json:"channel,omitempty"`
	Parts           []string `json:"parts"`
}

type bridgeChatMetadata struct {
	Schema             string `json:"schema"`
	CreatedByOperation string `json:"created_by_operation,omitempty"`
	LastOperation      string `json:"last_operation,omitempty"`
}

func requestBody(snapshot Snapshot, operationID string, create bool) ([]byte, error) {
	var chat map[string]json.RawMessage
	if err := decodeSingleJSON(snapshot.RawChat, &chat); err != nil {
		return nil, errors.New("target snapshot raw chat is invalid")
	}
	metadata := bridgeChatMetadata{Schema: bridgeSchema, LastOperation: operationID}
	if raw, ok := chat[bridgeMetadataKey]; ok {
		_ = json.Unmarshal(raw, &metadata)
		metadata.Schema = bridgeSchema
		metadata.LastOperation = operationID
	}
	if create {
		metadata.CreatedByOperation = operationID
	}
	setJSON(chat, bridgeMetadataKey, metadata)
	return json.Marshal(struct {
		Chat map[string]json.RawMessage `json:"chat"`
	}{Chat: chat})
}

func hasOperationMarker(rawChat json.RawMessage, operationID string, create bool) bool {
	var chat map[string]json.RawMessage
	if json.Unmarshal(rawChat, &chat) != nil {
		return false
	}
	var metadata bridgeChatMetadata
	if json.Unmarshal(chat[bridgeMetadataKey], &metadata) != nil || metadata.Schema != bridgeSchema {
		return false
	}
	if metadata.LastOperation != operationID {
		return false
	}
	return !create || metadata.CreatedByOperation == operationID
}

// HasOperationMarker proves that a readback contains the marker for the exact
// persisted operation. Recovery must combine this with MatchSnapshot before it
// credits an outcome that was previously unknown.
func HasOperationMarker(snapshot Snapshot, operationID string, create bool) bool {
	return hasOperationMarker(snapshot.RawChat, operationID, create)
}

func decodeSnapshot(raw []byte) (Snapshot, error) {
	var envelope struct {
		ID        string          `json:"id"`
		UserID    string          `json:"user_id"`
		Title     string          `json:"title"`
		Chat      json.RawMessage `json:"chat"`
		Archived  bool            `json:"archived"`
		Pinned    bool            `json:"pinned"`
		FolderID  *string         `json:"folder_id"`
		UpdatedAt int64           `json:"updated_at"`
	}
	if err := decodeSingleJSON(raw, &envelope); err != nil {
		return Snapshot{}, errors.New("malformed chat response")
	}
	if envelope.ID == "" || envelope.UserID == "" || len(envelope.Chat) == 0 {
		return Snapshot{}, errors.New("chat response is missing identity or content")
	}
	normalized, err := normalizeChat(envelope.Chat)
	if err != nil {
		return Snapshot{}, err
	}
	if envelope.Title != normalized.Title {
		return Snapshot{}, errors.New("chat response title disagrees with chat document")
	}
	snapshot := Snapshot{
		ChatID: envelope.ID, OwnerID: envelope.UserID, Archived: envelope.Archived,
		Pinned: envelope.Pinned, UpdatedAt: envelope.UpdatedAt,
		Normalized: normalized, RawChat: cloneRaw(envelope.Chat), RawEnvelope: cloneRaw(raw),
	}
	if envelope.FolderID != nil {
		snapshot.FolderID = *envelope.FolderID
	}
	snapshot.ConflictHash, err = hashRelevant(snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.Normalized.BusinessHash = snapshot.ConflictHash
	return snapshot, nil
}

func normalizeChat(rawChat json.RawMessage) (domain.TargetSnapshot, error) {
	var chat map[string]json.RawMessage
	if err := decodeSingleJSON(rawChat, &chat); err != nil {
		return domain.TargetSnapshot{}, errors.New("chat document is not one JSON object")
	}
	var title string
	if err := json.Unmarshal(chat["title"], &title); err != nil {
		return domain.TargetSnapshot{}, errors.New("chat document title is missing or invalid")
	}
	var history map[string]json.RawMessage
	if err := json.Unmarshal(chat["history"], &history); err != nil {
		return domain.TargetSnapshot{}, errors.New("chat history is missing or invalid")
	}
	var currentID string
	if err := json.Unmarshal(history["currentId"], &currentID); err != nil || currentID == "" {
		return domain.TargetSnapshot{}, errors.New("chat history currentId is missing or invalid")
	}
	var rawMessages map[string]json.RawMessage
	if err := json.Unmarshal(history["messages"], &rawMessages); err != nil || len(rawMessages) == 0 {
		return domain.TargetSnapshot{}, errors.New("chat history messages are missing or invalid")
	}

	type parsedMessage struct {
		ID, ParentID, Role, Content string
		Children                    []string
		Metadata                    bridgeMessageMetadata
	}
	parsed := make(map[string]parsedMessage, len(rawMessages))
	roots := make([]string, 0, 1)
	for key, raw := range rawMessages {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return domain.TargetSnapshot{}, errors.New("chat history contains a non-object message")
		}
		message := parsedMessage{}
		if json.Unmarshal(fields["id"], &message.ID) != nil || message.ID == "" || message.ID != key {
			return domain.TargetSnapshot{}, errors.New("chat history message key and id disagree")
		}
		if json.Unmarshal(fields["role"], &message.Role) != nil || message.Role == "" {
			return domain.TargetSnapshot{}, errors.New("chat history message role is missing or invalid")
		}
		if json.Unmarshal(fields["content"], &message.Content) != nil {
			return domain.TargetSnapshot{}, errors.New("chat history message content is not text")
		}
		if rawParent, exists := fields["parentId"]; exists && string(rawParent) != "null" {
			if json.Unmarshal(rawParent, &message.ParentID) != nil || message.ParentID == "" {
				return domain.TargetSnapshot{}, errors.New("chat history message parentId is invalid")
			}
		}
		if rawChildren, exists := fields["childrenIds"]; !exists || json.Unmarshal(rawChildren, &message.Children) != nil {
			return domain.TargetSnapshot{}, errors.New("chat history message childrenIds are missing or invalid")
		}
		if duplicateString(message.Children) {
			return domain.TargetSnapshot{}, errors.New("chat history message has duplicate children")
		}
		message.Metadata = readBridgeMetadata(fields)
		if message.ParentID == "" {
			roots = append(roots, key)
		}
		parsed[key] = message
	}
	if len(roots) != 1 {
		return domain.TargetSnapshot{}, errors.New("chat history must contain exactly one root")
	}
	for id, message := range parsed {
		if message.ParentID != "" {
			parent, exists := parsed[message.ParentID]
			if !exists || !slices.Contains(parent.Children, id) {
				return domain.TargetSnapshot{}, errors.New("chat history parent/child links disagree")
			}
		}
		for _, childID := range message.Children {
			child, exists := parsed[childID]
			if !exists || child.ParentID != id {
				return domain.TargetSnapshot{}, errors.New("chat history child/parent links disagree")
			}
		}
	}
	if _, exists := parsed[currentID]; !exists {
		return domain.TargetSnapshot{}, errors.New("chat history currentId does not exist")
	}
	if len(parsed[currentID].Children) != 0 {
		return domain.TargetSnapshot{}, errors.New("chat history currentId is not a leaf")
	}

	order := make([]string, 0, len(parsed))
	visiting := make(map[string]bool, len(parsed))
	visited := make(map[string]bool, len(parsed))
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return errors.New("chat history contains a cycle")
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		visited[id] = true
		order = append(order, id)
		for _, child := range parsed[id].Children {
			if err := visit(child); err != nil {
				return err
			}
		}
		visiting[id] = false
		return nil
	}
	if err := visit(roots[0]); err != nil {
		return domain.TargetSnapshot{}, err
	}
	if len(visited) != len(parsed) {
		return domain.TargetSnapshot{}, errors.New("chat history contains unreachable messages")
	}

	activeIDs := make([]string, 0, len(parsed))
	activeSeen := make(map[string]bool)
	for id := currentID; id != ""; id = parsed[id].ParentID {
		if activeSeen[id] {
			return domain.TargetSnapshot{}, errors.New("chat active branch contains a cycle")
		}
		activeSeen[id] = true
		activeIDs = append(activeIDs, id)
	}
	slices.Reverse(activeIDs)
	var active []map[string]json.RawMessage
	if err := json.Unmarshal(chat["messages"], &active); err != nil {
		return domain.TargetSnapshot{}, errors.New("chat active messages view is missing or invalid")
	}
	if len(active) != len(activeIDs) {
		return domain.TargetSnapshot{}, errors.New("chat active messages view length disagrees with current branch")
	}
	for index, fields := range active {
		var id, role, content, parentID string
		var children []string
		if json.Unmarshal(fields["id"], &id) != nil || json.Unmarshal(fields["role"], &role) != nil ||
			json.Unmarshal(fields["content"], &content) != nil || id != activeIDs[index] ||
			role != parsed[id].Role || content != parsed[id].Content {
			return domain.TargetSnapshot{}, errors.New("chat active messages view disagrees with history")
		}
		if rawParent, exists := fields["parentId"]; exists && string(rawParent) != "null" {
			if json.Unmarshal(rawParent, &parentID) != nil {
				return domain.TargetSnapshot{}, errors.New("chat active messages parentId is invalid")
			}
		}
		if json.Unmarshal(fields["childrenIds"], &children) != nil || parentID != parsed[id].ParentID ||
			!slices.Equal(children, parsed[id].Children) {
			return domain.TargetSnapshot{}, errors.New("chat active messages graph disagrees with history")
		}
	}

	normalized := domain.TargetSnapshot{Title: title, CurrentMessageID: currentID}
	for index, id := range order {
		message := parsed[id]
		parts := []string{message.Content}
		managed := message.Metadata.Schema == bridgeSchema && message.Metadata.SourceMessageID != ""
		if managed && strings.Join(message.Metadata.Parts, "") == message.Content {
			parts = append([]string(nil), message.Metadata.Parts...)
		}
		normalized.Messages = append(normalized.Messages, domain.TargetMessage{
			ID: id, SourceID: message.Metadata.SourceMessageID, Role: message.Role,
			Channel: message.Metadata.Channel, Parts: parts, Managed: managed, Order: index,
		})
	}
	return normalized, nil
}

func hashRelevant(snapshot Snapshot) (string, error) {
	var value any
	if len(snapshot.RawEnvelope) > 0 {
		var envelope map[string]any
		if err := decodeJSONUseNumber(snapshot.RawEnvelope, &envelope); err != nil {
			return "", errors.New("target response envelope is invalid")
		}
		var chat any
		if err := decodeJSONUseNumber(snapshot.RawChat, &chat); err != nil {
			return "", errors.New("target chat document is invalid")
		}
		// BuildSnapshot intentionally carries the previous response envelope so
		// stable unknown fields have an expected value. Replace the fields that
		// this desired write changes before hashing it.
		envelope["chat"] = chat
		envelope["title"] = snapshot.Normalized.Title
		envelope["current_message_id"] = snapshot.Normalized.CurrentMessageID
		envelope["archived"] = snapshot.Archived
		envelope["pinned"] = snapshot.Pinned
		if snapshot.FolderID == "" {
			envelope["folder_id"] = nil
		} else {
			envelope["folder_id"] = snapshot.FolderID
		}
		// These fields are computed or clock driven. All other known and
		// unknown response fields are deliberately conflict relevant.
		delete(envelope, "created_at")
		delete(envelope, "updated_at")
		delete(envelope, "last_read_at")
		delete(envelope, "context_usage")
		value = envelope
	} else {
		var chat any
		if err := decodeJSONUseNumber(snapshot.RawChat, &chat); err != nil {
			return "", errors.New("target chat document is invalid")
		}
		value = map[string]any{
			"chat": chat, "user_id": snapshot.OwnerID, "archived": snapshot.Archived,
			"pinned": snapshot.Pinned, "folder_id": snapshot.FolderID,
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("hash target snapshot: %w", err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func comparableChats(actualRaw, expectedRaw json.RawMessage) ([]byte, []byte, error) {
	var actual, expected map[string]any
	if err := decodeJSONUseNumber(actualRaw, &actual); err != nil {
		return nil, nil, err
	}
	if err := decodeJSONUseNumber(expectedRaw, &expected); err != nil {
		return nil, nil, err
	}
	delete(actual, "id")
	delete(expected, "id")
	expectedMarker, hasExpectedMarker := expected[bridgeMetadataKey].(map[string]any)
	if !hasExpectedMarker || expectedMarker["created_by_operation"] == nil || expectedMarker["created_by_operation"] == "" {
		// A pre-create desired snapshot cannot know the server receipt marker.
		delete(actual, bridgeMetadataKey)
		delete(expected, bridgeMetadataKey)
	} else {
		if actualMarker, ok := actual[bridgeMetadataKey].(map[string]any); ok {
			delete(actualMarker, "last_operation")
		}
		delete(expectedMarker, "last_operation")
	}
	actualCanonical, err := json.Marshal(actual)
	if err != nil {
		return nil, nil, err
	}
	expectedCanonical, err := json.Marshal(expected)
	return actualCanonical, expectedCanonical, err
}

func comparableEnvelope(raw json.RawMessage) ([]byte, error) {
	var envelope map[string]any
	if err := decodeJSONUseNumber(raw, &envelope); err != nil {
		return nil, err
	}
	delete(envelope, "chat")
	delete(envelope, "title")
	delete(envelope, "current_message_id")
	delete(envelope, "created_at")
	delete(envelope, "updated_at")
	delete(envelope, "last_read_at")
	delete(envelope, "context_usage")
	return json.Marshal(envelope)
}

func decodeJSONUseNumber(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func deterministicMessageID(source domain.SourceSnapshot, sourceMessageID string) string {
	basis := source.Identity.CandidateID
	if basis == "" {
		basis = source.ShareID
	}
	digest := sha256.Sum256([]byte("gpt-owu-gate/message/v1\x00" + basis + "\x00" + sourceMessageID))
	b := append([]byte(nil), digest[:16]...)
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func preservedUnmanagedChildren(message map[string]json.RawMessage, managedIDs map[string]struct{}) []string {
	var children []string
	_ = json.Unmarshal(message["childrenIds"], &children)
	return slices.DeleteFunc(children, func(id string) bool {
		_, managed := managedIDs[id]
		return managed
	})
}

func readBridgeMetadata(message map[string]json.RawMessage) bridgeMessageMetadata {
	var metadata bridgeMessageMetadata
	_ = json.Unmarshal(message[bridgeMetadataKey], &metadata)
	return metadata
}

func setJSON(object map[string]json.RawMessage, key string, value any) {
	raw, _ := json.Marshal(value)
	object[key] = raw
}

func duplicateString(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func cloneRaw(value []byte) json.RawMessage { return append(json.RawMessage(nil), value...) }

func equalNormalized(left, right domain.TargetSnapshot) bool {
	left.OfflineSimulation, right.OfflineSimulation = false, false
	left.BusinessHash, right.BusinessHash = "", ""
	leftRaw, leftErr := json.Marshal(left)
	rightRaw, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftRaw) == string(rightRaw)
}
