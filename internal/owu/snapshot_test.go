package owu

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
)

func TestBuildSnapshotPreservesTextPartsAndStableSeparateIDs(t *testing.T) {
	source := testSource("Title", "question", "answer")
	source.Messages[0].Parts = []string{"first", "\nsecond"}
	source.Messages[1].Channel = "commentary"
	first, err := BuildSnapshot(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildSnapshot(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !MatchSnapshot(first, second) {
		t.Fatal("same source did not produce a deterministic target")
	}
	if first.Normalized.Messages[0].ID == source.Messages[0].ID {
		t.Fatal("source and target message identities were conflated")
	}
	if got := first.Normalized.Messages[0].Parts; len(got) != 2 || got[0] != "first" || got[1] != "\nsecond" {
		t.Fatalf("source parts were not preserved: %#v", got)
	}
	if first.Normalized.Messages[1].Channel != "commentary" || first.Normalized.CurrentMessageID != first.Normalized.Messages[1].ID {
		t.Fatalf("channel/current node lost: %#v", first.Normalized)
	}
}

func TestBuildSnapshotUsesExplicitPlaceholderForMissingTitle(t *testing.T) {
	source := testSource("", "q", "a")
	snapshot, err := BuildSnapshot(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Normalized.Title != "Imported ChatGPT conversation" {
		t.Fatalf("unexpected missing-title behavior: %q", snapshot.Normalized.Title)
	}
}

func TestBuildSnapshotPreservesUnknownMetadataAndNativeBranch(t *testing.T) {
	initial, err := BuildSnapshot(testSource("Title", "q", "a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(initial.RawChat, &chat); err != nil {
		t.Fatal(err)
	}
	chat["future_option"] = map[string]any{"enabled": true}
	history := chat["history"].(map[string]any)
	messages := history["messages"].(map[string]any)
	rootID := initial.Normalized.Messages[0].ID
	root := messages[rootID].(map[string]any)
	root["future_message_field"] = "preserve-me"
	root["childrenIds"] = append(root["childrenIds"].([]any), "native-1")
	chat["messages"].([]any)[0].(map[string]any)["childrenIds"] = append(
		chat["messages"].([]any)[0].(map[string]any)["childrenIds"].([]any), "native-1",
	)
	messages["native-1"] = map[string]any{
		"id": "native-1", "parentId": rootID, "childrenIds": []any{},
		"role": "assistant", "content": "native branch", "native_meta": 7,
	}
	chatRaw := mustJSON(t, chat)
	current := decodeTestSnapshot(t, "chat-1", "account-1", chatRaw, map[string]any{"server_future": "keep"})
	if len(current.Normalized.Messages) != 3 || current.Normalized.Messages[2].Managed {
		t.Fatalf("native branch was not retained as unmanaged: %#v", current.Normalized.Messages)
	}

	updatedSource := testSource("New", "changed", "a")
	updated, err := BuildSnapshot(updatedSource, &current)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ConflictHash == current.ConflictHash {
		t.Fatal("desired conflict hash still described the old envelope chat")
	}
	var updatedChat map[string]any
	if err := json.Unmarshal(updated.RawChat, &updatedChat); err != nil {
		t.Fatal(err)
	}
	if updatedChat["future_option"].(map[string]any)["enabled"] != true {
		t.Fatal("unknown chat metadata was dropped")
	}
	updatedMessages := updatedChat["history"].(map[string]any)["messages"].(map[string]any)
	if _, ok := updatedMessages["native-1"]; !ok {
		t.Fatal("native branch was dropped")
	}
	if updatedMessages[rootID].(map[string]any)["future_message_field"] != "preserve-me" {
		t.Fatal("unknown per-message metadata was dropped")
	}
	if len(updated.RawEnvelope) == 0 {
		t.Fatal("unknown response envelope was not carried into expected readback")
	}
}

func TestDecodeSnapshotRejectsMalformedOrDivergentGraph(t *testing.T) {
	base, err := BuildSnapshot(testSource("Title", "q", "a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "missing current",
			mutate: func(chat map[string]any) {
				chat["history"].(map[string]any)["currentId"] = "missing"
			},
		},
		{
			name: "parent child disagreement",
			mutate: func(chat map[string]any) {
				messages := chat["history"].(map[string]any)["messages"].(map[string]any)
				messages[base.Normalized.Messages[0].ID].(map[string]any)["childrenIds"] = []any{}
			},
		},
		{
			name: "map key and id disagreement",
			mutate: func(chat map[string]any) {
				messages := chat["history"].(map[string]any)["messages"].(map[string]any)
				messages[base.Normalized.Messages[0].ID].(map[string]any)["id"] = "other"
			},
		},
		{
			name: "active view disagreement",
			mutate: func(chat map[string]any) {
				active := chat["messages"].([]any)
				active[0].(map[string]any)["content"] = "different"
			},
		},
		{
			name: "current node is not leaf",
			mutate: func(chat map[string]any) {
				chat["history"].(map[string]any)["currentId"] = base.Normalized.Messages[0].ID
				chat["messages"] = chat["messages"].([]any)[:1]
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var chat map[string]any
			if err := json.Unmarshal(base.RawChat, &chat); err != nil {
				t.Fatal(err)
			}
			test.mutate(chat)
			raw := testEnvelope(t, "chat-1", "account-1", mustJSON(t, chat), nil)
			if _, err := decodeSnapshot(raw); err == nil {
				t.Fatal("malformed target graph was accepted")
			}
		})
	}
}

func TestNativeContentEditWithStalePartsBecomesConflictData(t *testing.T) {
	expected, err := BuildSnapshot(testSource("Title", "q", "a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(expected.RawChat, &chat); err != nil {
		t.Fatal(err)
	}
	firstID := expected.Normalized.Messages[0].ID
	chat["history"].(map[string]any)["messages"].(map[string]any)[firstID].(map[string]any)["content"] = "native edit"
	chat["messages"].([]any)[0].(map[string]any)["content"] = "native edit"
	actual := decodeTestSnapshot(t, "chat", "owner", mustJSON(t, chat), nil)
	if !actual.Normalized.Messages[0].Managed || len(actual.Normalized.Messages[0].Parts) != 1 ||
		actual.Normalized.Messages[0].Parts[0] != "native edit" {
		t.Fatalf("native content edit was hidden or rejected: %#v", actual.Normalized.Messages[0])
	}
	if MatchSnapshot(actual, expected) {
		t.Fatal("native content edit matched the expected target")
	}
}

func TestConflictHashIsCanonicalAndCoversUnknownEnvelopeMetadata(t *testing.T) {
	chatA := json.RawMessage(`{"title":"T","history":{"currentId":"m","messages":{"m":{"id":"m","parentId":null,"childrenIds":[],"role":"user","content":"q"}}},"messages":[{"id":"m","parentId":null,"childrenIds":[],"role":"user","content":"q"}]}`)
	chatB := json.RawMessage(`{"messages":[{"content":"q","role":"user","childrenIds":[],"parentId":null,"id":"m"}],"history":{"messages":{"m":{"content":"q","role":"user","childrenIds":[],"parentId":null,"id":"m"}},"currentId":"m"},"title":"T"}`)
	rawA := []byte(`{"id":"chat","user_id":"owner","title":"T","chat":` + string(chatA) + `,"archived":false,"pinned":false,"folder_id":null,"meta":{"future":1},"created_at":1,"updated_at":2,"context_usage":{"x":1}}`)
	rawB := []byte(`{"context_usage":{"x":99},"updated_at":200,"created_at":100,"meta":{"future":1},"folder_id":null,"pinned":false,"archived":false,"chat":` + string(chatB) + `,"title":"T","user_id":"owner","id":"chat"}`)
	left, err := decodeSnapshot(rawA)
	if err != nil {
		t.Fatal(err)
	}
	right, err := decodeSnapshot(rawB)
	if err != nil {
		t.Fatal(err)
	}
	if !Equivalent(left, right) {
		t.Fatalf("canonical semantic equality failed: %s != %s", left.ConflictHash, right.ConflictHash)
	}
	changedRaw := strings.Replace(string(rawB), `"future":1`, `"future":2`, 1)
	changed, err := decodeSnapshot([]byte(changedRaw))
	if err != nil {
		t.Fatal(err)
	}
	if Equivalent(left, changed) {
		t.Fatal("unknown response metadata change was ignored")
	}
}

func TestMatchSnapshotIsStrictAboutGraphMetadataAndArchive(t *testing.T) {
	expected, err := BuildSnapshot(testSource("Title", "q", "a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	actual := expected
	actual.RawChat = setChatField(t, actual.RawChat, "future_option", "changed")
	if MatchSnapshot(actual, expected) {
		t.Fatal("unknown chat metadata change passed post-write verification")
	}
	actual = expected
	actual.Archived = true
	if MatchSnapshot(actual, expected) {
		t.Fatal("archive change passed post-write verification")
	}

	// A create receipt may add a server chat id and operation marker, but no
	// other graph/content changes are allowed.
	request, err := requestBody(expected, "op", true)
	if err != nil {
		t.Fatal(err)
	}
	var wrapped struct {
		Chat json.RawMessage `json:"chat"`
	}
	if err := json.Unmarshal(request, &wrapped); err != nil {
		t.Fatal(err)
	}
	actual = expected
	actual.RawChat = setChatField(t, wrapped.Chat, "id", "server-chat-id")
	if !MatchSnapshot(actual, expected) {
		t.Fatal("documented receipt-only fields prevented strict match")
	}
}

func TestJSONCanonicalizerRejectsTrailingGarbage(t *testing.T) {
	var value any
	if err := decodeJSONUseNumber([]byte(`{} trailing`), &value); err == nil {
		t.Fatal("trailing malformed JSON was accepted")
	}
}

func decodeTestSnapshot(t *testing.T, chatID, owner string, chat json.RawMessage, extras map[string]any) Snapshot {
	t.Helper()
	snapshot, err := decodeSnapshot(testEnvelope(t, chatID, owner, chat, extras))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func testEnvelope(t *testing.T, chatID, owner string, chat json.RawMessage, extras map[string]any) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(chat, &document); err != nil {
		t.Fatal(err)
	}
	envelope := map[string]any{
		"id": chatID, "user_id": owner, "title": document["title"], "chat": document,
		"archived": false, "pinned": false, "folder_id": nil, "created_at": 1, "updated_at": 2,
	}
	for key, value := range extras {
		envelope[key] = value
	}
	return mustJSON(t, envelope)
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBuildRejectsDuplicateOrUnsupportedSource(t *testing.T) {
	tests := []domain.SourceSnapshot{
		{},
		testSource("T", "q", "a"),
		testSource("T", "q", "a"),
		testSource("T", "q", "a"),
	}
	tests[1].Messages[1].ID = tests[1].Messages[0].ID
	tests[2].Messages[1].Role = "tool"
	tests[3].Identity.CandidateID = ""
	tests[3].ShareID = ""
	for _, source := range tests {
		if _, err := BuildSnapshot(source, nil); err == nil {
			t.Fatal("invalid source snapshot was accepted")
		}
	}
}
