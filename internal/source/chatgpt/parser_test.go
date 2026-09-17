package chatgpt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseSyntheticFixturePreservesVisibleContent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "synthetic-share.html"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ParseHTML(raw, ParseOptions{FetchedAt: time.Unix(1_800_000_000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Title != "M1 合成分享：格式核对" || snapshot.ShareID != "share-synthetic-001" {
		t.Fatalf("wrong identity fields: %#v", snapshot)
	}
	if snapshot.Identity.Evidence != "candidate" {
		t.Fatalf("backing id must remain candidate evidence: %#v", snapshot.Identity)
	}
	if len(snapshot.Messages) != 5 || snapshot.Coverage.ExcludedInternalNodes != 1 {
		t.Fatalf("wrong visible/internal counts: messages=%d coverage=%#v", len(snapshot.Messages), snapshot.Coverage)
	}
	wantRoles := []string{"user", "assistant", "user", "assistant", "assistant"}
	for index, role := range wantRoles {
		if snapshot.Messages[index].Role != role {
			t.Fatalf("message %d role=%q want=%q", index, snapshot.Messages[index].Role, role)
		}
	}
	first := strings.Join(snapshot.Messages[0].Parts, "")
	for _, exact := range []string{"中文", "```go\nfmt.Println(\"你好\")\n```", "$a^2+b^2=c^2$"} {
		if !strings.Contains(first, exact) {
			t.Fatalf("first message did not preserve %q: %q", exact, first)
		}
	}
	last := strings.Join(snapshot.Messages[len(snapshot.Messages)-1].Parts, "")
	if last != "完成：\n\n$$x+y=z$$" {
		t.Fatalf("last message changed: %q", last)
	}
	if snapshot.BusinessHash == "" || snapshot.Coverage.Status != "supported_path_complete" {
		t.Fatalf("unexpected completeness/hash: %#v", snapshot.Coverage)
	}
	again, err := ParseHTML(raw, ParseOptions{FetchedAt: time.Unix(1_900_000_000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if again.BusinessHash != snapshot.BusinessHash {
		t.Fatalf("fetch time changed business hash: %q != %q", again.BusinessHash, snapshot.BusinessHash)
	}
}

func TestParserRejectsUnknownInvalidAndCyclicReferences(t *testing.T) {
	tests := map[string][]byte{
		"unknown": []byte("<html><body>no table</body></html>"),
		"out of range": wrapTable(t, []any{
			map[string]any{"_1": 99, "_2": 99, "_3": 99}, "mapping", "linear_conversation", "current_node",
		}),
		"reference cycle": wrapTable(t, []any{
			map[string]any{"_1": 0, "_2": 0, "_3": 0}, "mapping", "linear_conversation", "current_node",
		}),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseHTML(raw, ParseOptions{}); err == nil {
				t.Fatal("expected parse error")
			}
		})
	}
}

func TestParserRejectsBrokenParentChain(t *testing.T) {
	root := testNode("root", "u1", nil)
	user := testNode("u1", "root", testMessage("m1", "user", "hello"))
	root["children"] = []any{"u1"}
	user["children"] = []any{}
	conversation := map[string]any{
		"mapping":             map[string]any{"root": root, "u1": user},
		"linear_conversation": []any{root, user}, "current_node": "u1", "title": "cycle",
	}
	if _, err := ParseHTML(encodeConversation(t, conversation), ParseOptions{}); err == nil || !strings.Contains(err.Error(), "cyclic parent") {
		t.Fatalf("expected cyclic parent error, got %v", err)
	}
}

func TestParserAcceptsEquivalentIndependentLinearNodes(t *testing.T) {
	conversation := consistentIndependentConversation()
	snapshot, err := ParseHTML(encodeConversation(t, conversation), ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 2 || snapshot.Coverage.Status != "supported_path_complete" {
		t.Fatalf("equivalent independent nodes should parse: %#v", snapshot)
	}
}

func TestInternalAssistantPayloadsAreExcludedWithoutHiddenFlag(t *testing.T) {
	for _, kind := range []string{"model_editable_context", "reasoning_recap", "unknown_new_type"} {
		for _, role := range []string{"assistant", "user"} {
			t.Run(role+"/"+kind, func(t *testing.T) {
				user := testNode("u", nil, testMessage("m-u", "user", "synthetic question"))
				internalMessage := testMessage("m-i", role, "SYNTHETIC-INTERNAL-CANARY")
				internalMessage["content"].(map[string]any)["content_type"] = kind
				internal := testNode("i", "u", internalMessage)
				answer := testNode("a", "i", testMessage("m-a", "assistant", "synthetic answer"))
				user["children"] = []any{"i"}
				internal["children"] = []any{"a"}
				conversation := map[string]any{"mapping": map[string]any{"u": user, "i": internal, "a": answer}, "linear_conversation": []any{user, internal, answer}, "current_node": "a"}
				snapshot, err := ParseHTML(encodeConversation(t, conversation), ParseOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if len(snapshot.Messages) != 2 {
					t.Fatal("non-text payload became transcript content")
				}
				excluded := kind != "unknown_new_type"
				if excluded != (snapshot.Coverage.Status == "supported_path_complete") {
					t.Fatal("wrong coverage classification")
				}
				if excluded && snapshot.Coverage.ExcludedInternalNodes != 1 {
					t.Fatal("missing internal exclusion count")
				}
				serialized, _ := json.Marshal(snapshot)
				if strings.Contains(string(serialized), "SYNTHETIC-INTERNAL-CANARY") {
					t.Fatal("internal payload leaked into snapshot")
				}
			})
		}
	}
}

func TestParserRejectsLinearMappingContradictions(t *testing.T) {
	tests := []struct {
		name       string
		field      string
		mutate     func(map[string]any)
		wantDetail string
	}{
		{
			name: "message body", field: "message",
			mutate: func(node map[string]any) {
				message := node["message"].(map[string]any)
				content := message["content"].(map[string]any)
				content["parts"] = []any{"contradictory body"}
			},
			wantDetail: "message differs",
		},
		{
			name: "missing parent", field: "parent",
			mutate: func(node map[string]any) {
				delete(node, "parent")
			},
			wantDetail: "parent differs",
		},
		{
			name: "children", field: "children",
			mutate: func(node map[string]any) {
				node["children"] = []any{}
			},
			wantDetail: "children differ",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conversation := consistentIndependentConversation()
			linear := conversation["linear_conversation"].([]any)
			test.mutate(linear[1].(map[string]any))
			_, err := ParseHTML(encodeConversation(t, conversation), ParseOptions{})
			if err == nil || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("contradictory linear %s should fail clearly, got %v", test.field, err)
			}
		})
	}
}

func TestUnsupportedVisibleContentIsReported(t *testing.T) {
	root := testNode("root", nil, nil)
	user := testNode("u1", "root", testMessage("m1", "user", "supported"))
	unsupportedMessage := testMessage("m2", "assistant", "ignored")
	unsupportedMessage["content"] = map[string]any{"content_type": "multimodal_text", "parts": []any{"ignored"}}
	assistant := testNode("a1", "u1", unsupportedMessage)
	root["children"], user["children"], assistant["children"] = []any{"u1"}, []any{"a1"}, []any{}
	conversation := map[string]any{
		"mapping":             map[string]any{"root": root, "u1": user, "a1": assistant},
		"linear_conversation": []any{root, user, assistant}, "current_node": "a1", "title": "unsupported",
	}
	snapshot, err := ParseHTML(encodeConversation(t, conversation), ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Coverage.Status != "partial" || len(snapshot.Coverage.Unsupported) != 1 || len(snapshot.Messages) != 1 {
		t.Fatalf("unsupported item was not reported: %#v", snapshot.Coverage)
	}
	blocked := false
	for _, diagnostic := range snapshot.Coverage.Diagnostics {
		blocked = blocked || diagnostic.Disposition == "blocked"
	}
	if !blocked {
		t.Fatalf("blocked diagnostic was not reported: %#v", snapshot.Coverage.Diagnostics)
	}
}

func TestAuxiliaryNodesAreIgnoredAndCitationsDegradeWithoutBlocking(t *testing.T) {
	root := testNode("root", nil, nil)
	system := testNode("sys", "root", testMessage("m-sys", "system", "internal"))
	user := testNode("u", "sys", testMessage("m-u", "user", "question"))
	tool := testNode("tool", "u", testMessage("m-tool", "tool", "search result"))
	thoughtMessage := testMessage("m-thought", "assistant", "internal reasoning")
	thoughtMessage["content"].(map[string]any)["content_type"] = "thoughts"
	thought := testNode("thought", "tool", thoughtMessage)
	browseMessage := testMessage("m-browse", "assistant", "search cards")
	browseMessage["content"].(map[string]any)["content_type"] = "tether_browsing_display"
	browse := testNode("browse", "thought", browseMessage)
	quoteMessage := testMessage("m-quote", "assistant", "quoted source")
	quoteMessage["content"].(map[string]any)["content_type"] = "tether_quote"
	quote := testNode("quote", "browse", quoteMessage)
	answerMessage := testMessage("m-a", "assistant", "answer citeturn1search0")
	answerMessage["channel"] = "final"
	answerMessage["metadata"] = map[string]any{
		"content_references": []any{map[string]any{
			"type": "grouped_webpages", "matched_text": "citeturn1search0",
			"items": []any{map[string]any{
				"title":       "Nested title must not replace the conversation title",
				"attribution": "Example", "url": "https://example.com/source",
			}},
		}},
	}
	answer := testNode("a", "quote", answerMessage)
	nodes := []map[string]any{root, system, user, tool, thought, browse, quote, answer}
	for index := 0; index+1 < len(nodes); index++ {
		nodes[index]["children"] = []any{nodes[index+1]["id"]}
	}
	mapping := make(map[string]any, len(nodes))
	linear := make([]any, 0, len(nodes))
	for _, node := range nodes {
		mapping[node["id"].(string)] = node
		linear = append(linear, node)
	}
	conversation := map[string]any{
		"mapping": mapping, "linear_conversation": linear, "current_node": "a",
		"title": "Expected conversation title",
	}
	snapshot, err := ParseHTML(encodeConversation(t, conversation), ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Title != "Expected conversation title" {
		t.Fatalf("nested citation title contaminated conversation title: %q", snapshot.Title)
	}
	if snapshot.Coverage.Status != "supported_path_complete" || len(snapshot.Coverage.Unsupported) != 0 {
		t.Fatalf("auxiliary nodes blocked coverage: %#v", snapshot.Coverage)
	}
	if len(snapshot.Messages) != 2 || strings.Join(snapshot.Messages[1].Parts, "") != "answer [Example](https://example.com/source)" {
		t.Fatalf("citation was not degraded into Markdown: %#v", snapshot.Messages)
	}
	ignored, degraded, blocked := 0, 0, 0
	for _, diagnostic := range snapshot.Coverage.Diagnostics {
		if diagnostic.SourceID == "" || diagnostic.ContentType == "" {
			t.Fatalf("diagnostic lacks source identity or content type: %#v", diagnostic)
		}
		switch diagnostic.Disposition {
		case "ignored":
			ignored++
		case "degraded":
			degraded++
		case "blocked":
			blocked++
		}
	}
	if ignored != 6 || degraded != 1 || blocked != 0 {
		t.Fatalf("wrong diagnostic dispositions: ignored=%d degraded=%d blocked=%d diagnostics=%#v", ignored, degraded, blocked, snapshot.Coverage.Diagnostics)
	}

	cleanRoot := testNode("root", nil, nil)
	cleanUser := testNode("u", "root", testMessage("m-u", "user", "question"))
	cleanAnswer := testNode("a", "u", answerMessage)
	cleanRoot["children"] = []any{"u"}
	cleanUser["children"] = []any{"a"}
	cleanConversation := map[string]any{
		"mapping":             map[string]any{"root": cleanRoot, "u": cleanUser, "a": cleanAnswer},
		"linear_conversation": []any{cleanRoot, cleanUser, cleanAnswer},
		"current_node":        "a", "title": "Expected conversation title",
	}
	cleanSnapshot, err := ParseHTML(encodeConversation(t, cleanConversation), ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cleanSnapshot.BusinessHash != snapshot.BusinessHash {
		t.Fatalf("ignored auxiliary nodes changed the semantic business hash: %s != %s", cleanSnapshot.BusinessHash, snapshot.BusinessHash)
	}
}

func FuzzParseHTML(f *testing.F) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "synthetic-share.html"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(fixture)
	f.Add([]byte("not html"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		_, _ = ParseHTML(raw, ParseOptions{})
	})
}

func testNode(id string, parent any, message any) map[string]any {
	return map[string]any{"id": id, "parent": parent, "children": []any{}, "message": message}
}

func testMessage(id, role, text string) map[string]any {
	return map[string]any{
		"id": id, "author": map[string]any{"role": role},
		"content":   map[string]any{"content_type": "text", "parts": []any{text}},
		"recipient": "all", "metadata": map[string]any{},
	}
}

func consistentIndependentConversation() map[string]any {
	mappingRoot := testNode("root", nil, nil)
	mappingUser := testNode("u1", "root", testMessage("m1", "user", "question"))
	mappingAssistant := testNode("a1", "u1", testMessage("m2", "assistant", "answer"))
	mappingRoot["children"] = []any{"u1"}
	mappingUser["children"] = []any{"a1"}

	linearRoot := testNode("root", nil, nil)
	linearUser := testNode("u1", "root", testMessage("m1", "user", "question"))
	linearAssistant := testNode("a1", "u1", testMessage("m2", "assistant", "answer"))
	linearRoot["children"] = []any{"u1"}
	linearUser["children"] = []any{"a1"}

	return map[string]any{
		"mapping": map[string]any{
			"root": mappingRoot, "u1": mappingUser, "a1": mappingAssistant,
		},
		"linear_conversation": []any{linearRoot, linearUser, linearAssistant},
		"current_node":        "a1",
		"title":               "independent nodes",
	}
}

func encodeConversation(t *testing.T, conversation map[string]any) []byte {
	t.Helper()
	encoder := &testRefEncoder{}
	if root := encoder.encode(reflect.ValueOf(conversation)); root != 0 {
		t.Fatalf("root reference=%d want=0", root)
	}
	return wrapTable(t, encoder.table)
}

type testRefEncoder struct {
	table []any
}

func (e *testRefEncoder) encode(value reflect.Value) int {
	if !value.IsValid() || (value.Kind() == reflect.Interface && value.IsNil()) {
		return -5
	}
	if value.Kind() == reflect.Interface {
		return e.encode(value.Elem())
	}
	switch value.Kind() {
	case reflect.Map:
		index := len(e.table)
		e.table = append(e.table, nil)
		object := make(map[string]any)
		iterator := value.MapRange()
		for iterator.Next() {
			keyIndex := e.encode(iterator.Key())
			object["_"+strconv.Itoa(keyIndex)] = e.encode(iterator.Value())
		}
		e.table[index] = object
		return index
	case reflect.Slice:
		index := len(e.table)
		e.table = append(e.table, nil)
		items := make([]any, value.Len())
		for i := range value.Len() {
			items[i] = e.encode(value.Index(i))
		}
		e.table[index] = items
		return index
	case reflect.String:
		index := len(e.table)
		e.table = append(e.table, value.String())
		return index
	case reflect.Bool:
		index := len(e.table)
		e.table = append(e.table, value.Bool())
		return index
	case reflect.Int, reflect.Int64:
		index := len(e.table)
		e.table = append(e.table, value.Int())
		return index
	default:
		panic("unsupported test value: " + value.Kind().String())
	}
}

func wrapTable(t *testing.T, table []any) []byte {
	t.Helper()
	payload, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	argument, err := json.Marshal(string(payload))
	if err != nil {
		t.Fatal(err)
	}
	return []byte("<script>" + streamMarker + string(argument) + ")</script>")
}
