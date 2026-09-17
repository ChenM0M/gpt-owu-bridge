// Package chatgpt parses the bounded, non-executed reference table observed in
// ChatGPT share HTML into transport-independent domain snapshots.
package chatgpt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
)

const streamMarker = `window.__reactRouterContext.streamController.enqueue(`

type Limits struct {
	MaxHTMLBytes    int
	MaxTables       int
	MaxTableEntries int
	MaxDepth        int
	MaxNodes        int
	MaxParts        int
	MaxTextBytes    int
}

func DefaultLimits() Limits {
	return Limits{
		MaxHTMLBytes: 8 << 20, MaxTables: 16, MaxTableEntries: 100_000,
		MaxDepth: 512, MaxNodes: 10_000, MaxParts: 10_000, MaxTextBytes: 4 << 20,
	}
}

type ParseOptions struct {
	FetchedAt time.Time
	Limits    Limits
}

type candidate struct {
	table []any
	index int
}

func ParseHTML(raw []byte, options ParseOptions) (domain.SourceSnapshot, error) {
	limits := options.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if len(raw) == 0 {
		return domain.SourceSnapshot{}, formatError("empty share HTML")
	}
	if len(raw) > limits.MaxHTMLBytes {
		return domain.SourceSnapshot{}, formatError("share HTML exceeds size limit")
	}
	tables, err := extractTables(raw, limits)
	if err != nil {
		return domain.SourceSnapshot{}, err
	}
	var candidates []candidate
	for _, table := range tables {
		for index, value := range table {
			object, ok := value.(map[string]any)
			if !ok {
				continue
			}
			keys, err := encodedObjectKeys(table, object)
			if err != nil {
				return domain.SourceSnapshot{}, err
			}
			if keys["mapping"] && keys["linear_conversation"] && keys["current_node"] {
				candidates = append(candidates, candidate{table: table, index: index})
			}
		}
	}
	if len(candidates) != 1 {
		return domain.SourceSnapshot{}, formatError("ambiguous or missing conversation structure")
	}
	decoder := newRefDecoder(candidates[0].table, limits.MaxDepth)
	decoded, err := decoder.decode(candidates[0].index, 0)
	if err != nil {
		return domain.SourceSnapshot{}, err
	}
	conversation, ok := decoded.(map[string]any)
	if !ok {
		return domain.SourceSnapshot{}, formatError("conversation root is not an object")
	}
	fetchedAt := options.FetchedAt.UTC()
	if fetchedAt.IsZero() {
		fetchedAt = time.Now().UTC()
	}
	snapshot, err := buildSnapshot(conversation, fetchedAt, limits)
	if err != nil {
		return domain.SourceSnapshot{}, err
	}
	hash, err := snapshot.ComputeHash()
	if err != nil {
		return domain.SourceSnapshot{}, err
	}
	snapshot.BusinessHash = hash
	return snapshot, nil
}

func extractTables(raw []byte, limits Limits) ([][]any, error) {
	var tables [][]any
	rest := raw
	for {
		position := bytes.Index(rest, []byte(streamMarker))
		if position < 0 {
			break
		}
		rest = rest[position+len(streamMarker):]
		decoder := json.NewDecoder(bytes.NewReader(rest))
		var payload string
		if err := decoder.Decode(&payload); err != nil {
			return nil, formatError("invalid stream enqueue string")
		}
		consumed := int(decoder.InputOffset())
		if consumed >= len(rest) || !strings.HasPrefix(strings.TrimSpace(string(rest[consumed:])), ")") {
			return nil, formatError("unterminated stream enqueue call")
		}
		if strings.HasPrefix(strings.TrimSpace(payload), "[") {
			var table []any
			payloadDecoder := json.NewDecoder(strings.NewReader(payload))
			payloadDecoder.UseNumber()
			if err := payloadDecoder.Decode(&table); err != nil {
				return nil, formatError("invalid reference table JSON")
			}
			var trailing any
			if err := payloadDecoder.Decode(&trailing); err == nil {
				return nil, formatError("reference table contains trailing JSON")
			} else if !errors.Is(err, io.EOF) {
				return nil, formatError("reference table contains invalid trailing data")
			}
			if len(table) > limits.MaxTableEntries {
				return nil, formatError("reference table exceeds entry limit")
			}
			tables = append(tables, table)
			if len(tables) > limits.MaxTables {
				return nil, formatError("too many reference tables")
			}
		}
		rest = rest[consumed:]
	}
	if len(tables) == 0 {
		return nil, formatError("no supported reference table")
	}
	return tables, nil
}

func encodedObjectKeys(table []any, object map[string]any) (map[string]bool, error) {
	keys := make(map[string]bool, len(object))
	for encoded := range object {
		index, err := encodedKeyIndex(encoded)
		if err != nil || index < 0 || index >= len(table) {
			return nil, formatError("invalid encoded object key")
		}
		key, ok := table[index].(string)
		if !ok {
			return nil, formatError("encoded object key does not reference a string")
		}
		keys[key] = true
	}
	return keys, nil
}

func encodedKeyIndex(key string) (int, error) {
	if len(key) < 2 || key[0] != '_' {
		return 0, errors.New("unsupported object encoding")
	}
	index, err := strconv.Atoi(key[1:])
	if err != nil || index < 0 {
		return 0, errors.New("unsupported object encoding")
	}
	return index, nil
}

type refDecoder struct {
	table    []any
	maxDepth int
	state    []uint8
	memo     []any
}

func newRefDecoder(table []any, maxDepth int) *refDecoder {
	return &refDecoder{table: table, maxDepth: maxDepth, state: make([]uint8, len(table)), memo: make([]any, len(table))}
}

func (d *refDecoder) decode(index, depth int) (any, error) {
	if index == -5 {
		return nil, nil
	}
	if index < 0 || index >= len(d.table) {
		return nil, formatError("reference index is out of range")
	}
	if depth > d.maxDepth {
		return nil, formatError("reference depth limit exceeded")
	}
	if d.state[index] == 1 {
		return nil, formatError("cyclic reference table")
	}
	if d.state[index] == 2 {
		return d.memo[index], nil
	}
	d.state[index] = 1
	var result any
	switch value := d.table[index].(type) {
	case map[string]any:
		object := make(map[string]any, len(value))
		for encodedKey, encodedValue := range value {
			keyIndex, err := encodedKeyIndex(encodedKey)
			if err != nil {
				return nil, formatError("unsupported object encoding")
			}
			decodedKey, err := d.decode(keyIndex, depth+1)
			if err != nil {
				return nil, err
			}
			key, ok := decodedKey.(string)
			if !ok {
				return nil, formatError("object key is not a string")
			}
			valueIndex, err := referenceIndex(encodedValue)
			if err != nil {
				return nil, err
			}
			decodedValue, err := d.decode(valueIndex, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = decodedValue
		}
		result = object
	case []any:
		array := make([]any, 0, len(value))
		for _, encodedValue := range value {
			valueIndex, err := referenceIndex(encodedValue)
			if err != nil {
				return nil, err
			}
			decodedValue, err := d.decode(valueIndex, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, decodedValue)
		}
		result = array
	default:
		result = value
	}
	d.state[index] = 2
	d.memo[index] = result
	return result, nil
}

func referenceIndex(value any) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, formatError("reference is not an integer")
	}
	integer, err := strconv.Atoi(string(number))
	if err != nil {
		return 0, formatError("reference is not an integer")
	}
	return integer, nil
}

func buildSnapshot(conversation map[string]any, fetchedAt time.Time, limits Limits) (domain.SourceSnapshot, error) {
	mapping, ok := conversation["mapping"].(map[string]any)
	if !ok || len(mapping) == 0 {
		return domain.SourceSnapshot{}, formatError("mapping is missing or empty")
	}
	if len(mapping) > limits.MaxNodes {
		return domain.SourceSnapshot{}, formatError("mapping exceeds node limit")
	}
	for key, rawNode := range mapping {
		node, ok := rawNode.(map[string]any)
		if !ok {
			return domain.SourceSnapshot{}, formatError("mapping contains a non-object node")
		}
		id, _ := node["id"].(string)
		if id == "" || id != key {
			return domain.SourceSnapshot{}, formatError("mapping key and node id differ")
		}
	}
	linear, ok := conversation["linear_conversation"].([]any)
	if !ok || len(linear) == 0 {
		return domain.SourceSnapshot{}, formatError("linear conversation is missing or empty")
	}
	if len(linear) > limits.MaxNodes {
		return domain.SourceSnapshot{}, formatError("linear conversation exceeds node limit")
	}
	current, ok := conversation["current_node"].(string)
	if !ok || current == "" {
		return domain.SourceSnapshot{}, formatError("current node is missing")
	}

	linearIDs := make([]string, 0, len(linear))
	linearNodes := make([]map[string]any, 0, len(linear))
	for _, rawNode := range linear {
		node, ok := rawNode.(map[string]any)
		if !ok {
			return domain.SourceSnapshot{}, formatError("linear conversation contains a non-object node")
		}
		id, ok := node["id"].(string)
		if !ok || id == "" {
			return domain.SourceSnapshot{}, formatError("conversation node has no id")
		}
		mapped, ok := mapping[id].(map[string]any)
		if !ok {
			return domain.SourceSnapshot{}, formatError("linear conversation node is absent from mapping")
		}
		mappedID, _ := mapped["id"].(string)
		if mappedID != id {
			return domain.SourceSnapshot{}, formatError("mapping key and node id differ")
		}
		if err := validateLinearNode(mapped, node); err != nil {
			return domain.SourceSnapshot{}, err
		}
		linearIDs = append(linearIDs, id)
		// Mapping is the authority for ancestry and message extraction. The
		// independently decoded linear node is used only after proving that the
		// fields consumed by this parser agree with the mapped node.
		linearNodes = append(linearNodes, mapped)
	}
	chain, err := ancestry(mapping, current, limits.MaxDepth)
	if err != nil {
		return domain.SourceSnapshot{}, err
	}
	if !equalStrings(chain, linearIDs) {
		return domain.SourceSnapshot{}, formatError("linear conversation differs from current-node ancestry")
	}
	if err := validateChildren(mapping); err != nil {
		return domain.SourceSnapshot{}, err
	}

	coverage := domain.Coverage{
		Status: "supported_path_complete",
		Limitations: []string{
			"only the observed React Router reference-table format is supported",
			"attachments and alternate-branch fidelity are not yet verified",
		},
	}
	for id, rawNode := range mapping {
		node := rawNode.(map[string]any)
		children, _ := stringSlice(node["children"])
		if len(children) > 1 {
			coverage.Unsupported = append(coverage.Unsupported, domain.UnsupportedItem{
				NodeID: id, Kind: "alternate_branch", Detail: "mapping contains more than one child",
			})
		}
	}

	messages := make([]domain.Message, 0, len(linearNodes))
	messageIDs := make(map[string]struct{})
	totalParts, totalText := 0, 0
	for order, node := range linearNodes {
		nodeID := node["id"].(string)
		rawMessage, present := node["message"]
		if !present || rawMessage == nil {
			coverage.ExcludedNonMessageNodes++
			continue
		}
		messageObject, ok := rawMessage.(map[string]any)
		if !ok {
			return domain.SourceSnapshot{}, formatError("message is not an object")
		}
		author, _ := messageObject["author"].(map[string]any)
		role, _ := author["role"].(string)
		if role != "user" && role != "assistant" {
			coverage.ExcludedInternalNodes++
			continue
		}
		if hidden(messageObject) || internalRecipient(messageObject) {
			coverage.ExcludedInternalNodes++
			continue
		}
		content, ok := messageObject["content"].(map[string]any)
		contentType, _ := content["content_type"].(string)
		// Real share payloads include internal assistant context and reasoning
		// recap nodes without the visually-hidden metadata flag. They are not
		// transcript messages and must never be imported or echoed in previews.
		if role == "assistant" && (contentType == "model_editable_context" || contentType == "reasoning_recap") {
			coverage.ExcludedInternalNodes++
			continue
		}
		channel := ""
		if rawChannel, present := messageObject["channel"]; present && rawChannel != nil {
			var ok bool
			channel, ok = rawChannel.(string)
			if !ok {
				coverage.Unsupported = append(coverage.Unsupported, domain.UnsupportedItem{
					NodeID: nodeID, Kind: "channel", Detail: "visible role uses a non-string channel",
				})
				continue
			}
		}
		if channel != "" && channel != "final" && channel != "commentary" {
			coverage.Unsupported = append(coverage.Unsupported, domain.UnsupportedItem{
				NodeID: nodeID, Kind: "channel", Detail: "visible role uses an unsupported channel",
			})
			continue
		}
		if !ok || contentType != "text" {
			coverage.Unsupported = append(coverage.Unsupported, domain.UnsupportedItem{
				NodeID: nodeID, Kind: "content_type", Detail: "visible message is not supported text content",
			})
			continue
		}
		rawParts, ok := content["parts"].([]any)
		if !ok {
			coverage.Unsupported = append(coverage.Unsupported, domain.UnsupportedItem{
				NodeID: nodeID, Kind: "parts", Detail: "text parts are missing or not an array",
			})
			continue
		}
		parts := make([]string, 0, len(rawParts))
		partSupported := true
		for _, rawPart := range rawParts {
			part, ok := rawPart.(string)
			if !ok {
				partSupported = false
				break
			}
			parts = append(parts, part)
			totalText += len(part)
		}
		totalParts += len(rawParts)
		if totalParts > limits.MaxParts || totalText > limits.MaxTextBytes {
			return domain.SourceSnapshot{}, formatError("message content exceeds resource limits")
		}
		if !partSupported {
			coverage.Unsupported = append(coverage.Unsupported, domain.UnsupportedItem{
				NodeID: nodeID, Kind: "parts", Detail: "text message contains a non-string part",
			})
			continue
		}
		messageID, _ := messageObject["id"].(string)
		if messageID == "" {
			return domain.SourceSnapshot{}, formatError("visible message has no independent id")
		}
		if _, duplicate := messageIDs[messageID]; duplicate {
			return domain.SourceSnapshot{}, formatError("duplicate visible message id")
		}
		messageIDs[messageID] = struct{}{}
		parent, _ := node["parent"].(string)
		children, _ := stringSlice(node["children"])
		messages = append(messages, domain.Message{
			ID: messageID, NodeID: nodeID, Role: role, Channel: channel, Parts: parts,
			ParentNodeID: parent, ChildNodeIDs: children, Order: order,
			SourceCreatedAt: parseTime(messageObject["create_time"]),
		})
	}
	coverage.SelectedMessages = len(messages)
	slices.SortFunc(coverage.Unsupported, func(left, right domain.UnsupportedItem) int {
		if left.NodeID != right.NodeID {
			return strings.Compare(left.NodeID, right.NodeID)
		}
		return strings.Compare(left.Kind, right.Kind)
	})
	if len(coverage.Unsupported) > 0 {
		coverage.Status = "partial"
	}
	if len(messages) == 0 {
		return domain.SourceSnapshot{}, formatError("no supported user-facing messages")
	}

	title, err := optionalString(conversation, "title")
	if err != nil {
		return domain.SourceSnapshot{}, err
	}
	shareID, err := optionalString(conversation, "conversation_id")
	if err != nil {
		return domain.SourceSnapshot{}, err
	}
	backingID, err := optionalString(conversation, "backing_conversation_id")
	if err != nil {
		return domain.SourceSnapshot{}, err
	}
	identity := domain.SourceIdentity{Evidence: domain.EvidenceUnknown}
	if backingID != "" {
		identity = domain.SourceIdentity{
			CandidateID: backingID, Evidence: domain.EvidenceCandidate,
			Basis: "backing_conversation_id observed in share payload; lifecycle not verified",
		}
	}
	return domain.SourceSnapshot{
		SourceType: domain.SourceChatGPTShare, Identity: identity, ShareID: shareID,
		Title: title, Messages: messages, CurrentNodeID: current,
		SourceCreatedAt: parseTime(conversation["create_time"]), FetchedAt: fetchedAt,
		ParserVersion: domain.ParserVersion, Coverage: coverage,
	}, nil
}

func validateLinearNode(mapped, linear map[string]any) error {
	if !reflect.DeepEqual(mapped["parent"], linear["parent"]) {
		return formatError("linear conversation node parent differs from mapping")
	}
	if !reflect.DeepEqual(mapped["children"], linear["children"]) {
		return formatError("linear conversation node children differ from mapping")
	}
	if !reflect.DeepEqual(mapped["message"], linear["message"]) {
		return formatError("linear conversation node message differs from mapping")
	}
	return nil
}

func ancestry(mapping map[string]any, current string, maxDepth int) ([]string, error) {
	seen := make(map[string]struct{})
	var reversed []string
	for current != "" {
		if len(reversed) > maxDepth {
			return nil, formatError("parent chain exceeds depth limit")
		}
		if _, duplicate := seen[current]; duplicate {
			return nil, formatError("cyclic parent chain")
		}
		seen[current] = struct{}{}
		rawNode, ok := mapping[current]
		if !ok {
			return nil, formatError("parent chain references a missing node")
		}
		node, ok := rawNode.(map[string]any)
		if !ok {
			return nil, formatError("mapping contains a non-object node")
		}
		id, _ := node["id"].(string)
		if id != current {
			return nil, formatError("mapping key and node id differ")
		}
		reversed = append(reversed, current)
		parent, present := node["parent"]
		if !present || parent == nil {
			current = ""
			continue
		}
		parentID, ok := parent.(string)
		if !ok {
			return nil, formatError("node parent is not a string or null")
		}
		current = parentID
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed, nil
}

func validateChildren(mapping map[string]any) error {
	for id, rawNode := range mapping {
		node, ok := rawNode.(map[string]any)
		if !ok {
			return formatError("mapping contains a non-object node")
		}
		children, present := node["children"]
		if !present || children == nil {
			continue
		}
		childIDs, ok := stringSlice(children)
		if !ok {
			return formatError("node children are not strings")
		}
		for _, childID := range childIDs {
			rawChild, exists := mapping[childID]
			if !exists {
				return formatError("node child references a missing node")
			}
			child := rawChild.(map[string]any)
			parent, _ := child["parent"].(string)
			if parent != id {
				return formatError("child and parent references disagree")
			}
		}
	}
	for id, rawNode := range mapping {
		node := rawNode.(map[string]any)
		parentValue, present := node["parent"]
		if !present || parentValue == nil {
			continue
		}
		parentID, ok := parentValue.(string)
		if !ok || parentID == "" {
			return formatError("node parent is not a non-empty string or null")
		}
		rawParent, exists := mapping[parentID]
		if !exists {
			return formatError("node parent references a missing node")
		}
		parent := rawParent.(map[string]any)
		children, ok := stringSlice(parent["children"])
		if !ok || !slices.Contains(children, id) {
			return formatError("parent does not reference its child")
		}
	}
	return nil
}

func optionalString(object map[string]any, key string) (string, error) {
	value, present := object[key]
	if !present || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", formatError(key + " is not a string")
	}
	return text, nil
}

func hidden(message map[string]any) bool {
	metadata, _ := message["metadata"].(map[string]any)
	value, _ := metadata["is_visually_hidden_from_conversation"].(bool)
	return value
}

func internalRecipient(message map[string]any) bool {
	recipient, present := message["recipient"]
	if !present || recipient == nil {
		return false
	}
	value, ok := recipient.(string)
	return !ok || (value != "" && value != "all")
}

func stringSlice(value any) ([]string, bool) {
	if value == nil {
		return nil, true
	}
	raw, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		result = append(result, text)
	}
	return result, true
}

func parseTime(value any) *time.Time {
	var result time.Time
	switch typed := value.(type) {
	case json.Number:
		seconds, err := strconv.ParseFloat(string(typed), 64)
		if err != nil {
			return nil
		}
		whole := int64(seconds)
		nanos := int64((seconds - float64(whole)) * 1e9)
		result = time.Unix(whole, nanos).UTC()
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, typed)
		if err != nil {
			return nil
		}
		result = parsed.UTC()
	default:
		return nil
	}
	return &result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func formatError(message string) error {
	return &domain.ResultError{
		Code: "unsupported_source_format", Message: message,
		NextAction: "provide a supported, bounded share HTML snapshot", Retryable: false,
	}
}
