// Package owu implements the deliberately small Open WebUI HTTP contract used
// by the bridge. It has no list, search, delete, or arbitrary-request method.
package owu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	SupportedVersion       = "0.11.3"
	defaultMaxResponseSize = int64(16 << 20)
	defaultTimeout         = 15 * time.Second
)

// Config fixes the only Open WebUI origin and credential that the client can
// use. Expected identity fields are optional on first installation and should
// be populated from the persisted installation identity thereafter.
type Config struct {
	BaseURL           string
	Token             string
	ExpectedVersion   string
	ExpectedSiteID    string
	ExpectedAccountID string
	MaxResponseBytes  int64
	Timeout           time.Duration
	HTTPClient        *http.Client
}

type Client struct {
	baseURL           *url.URL
	token             string
	expectedVersion   string
	expectedSiteID    string
	expectedAccountID string
	maxResponseBytes  int64
	httpClient        *http.Client
}

type Identity struct {
	SiteID       string `json:"site_id"`
	DeploymentID string `json:"deployment_id"`
	AccountID    string `json:"account_id"`
	Version      string `json:"version"`
	Email        string `json:"email,omitempty"`
	Name         string `json:"name,omitempty"`
	Role         string `json:"role,omitempty"`
}

type CreateRequest struct {
	OperationID string
	Snapshot    Snapshot
}

type CreateReceipt struct {
	TargetID string
	Snapshot Snapshot
}

type UpdateRequest struct {
	OperationID string
	Snapshot    Snapshot
}

type UpdateReceipt struct {
	Snapshot Snapshot
}

// HTTPError is intentionally body-free: Open WebUI error bodies and request
// URLs can contain deployment details that must not escape through callers.
type HTTPError struct {
	Operation  string
	StatusCode int
}

func (e *HTTPError) Error() string {
	if e.StatusCode == 0 {
		return "Open WebUI " + e.Operation + " failed"
	}
	return fmt.Sprintf("Open WebUI %s failed with HTTP %d", e.Operation, e.StatusCode)
}

// OutcomeUnknownError means that a write was sent but a trustworthy receipt
// was not obtained. A caller must reconcile; it must not retry the write.
type OutcomeUnknownError struct {
	Operation string
	Cause     string
}

func (e *OutcomeUnknownError) Error() string {
	return "Open WebUI " + e.Operation + " outcome is unknown; reconciliation is required"
}

func IsOutcomeUnknown(err error) bool {
	var target *OutcomeUnknownError
	return errors.As(err, &target)
}

func New(config Config) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errors.New("Open WebUI base URL is invalid")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, errors.New("Open WebUI base URL must use http or https")
	}
	if base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("Open WebUI base URL must not contain credentials, query, or fragment")
	}
	base.Path = strings.TrimRight(base.Path, "/")
	base.RawPath = ""

	token := strings.TrimSpace(config.Token)
	if token == "" {
		return nil, errors.New("Open WebUI token is required")
	}
	if strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("Open WebUI token is invalid")
	}

	version := strings.TrimPrefix(strings.TrimSpace(config.ExpectedVersion), "v")
	if version == "" {
		version = SupportedVersion
	}
	if version != SupportedVersion {
		return nil, fmt.Errorf("Open WebUI adapter supports only version %s", SupportedVersion)
	}
	maxBytes := config.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxResponseSize
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	baseClient := config.HTTPClient
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	clientCopy := *baseClient
	clientCopy.Timeout = timeout
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("Open WebUI redirects are disabled")
	}

	return &Client{
		baseURL: base, token: token, expectedVersion: version,
		expectedSiteID: config.ExpectedSiteID, expectedAccountID: config.ExpectedAccountID,
		maxResponseBytes: maxBytes, httpClient: &clientCopy,
	}, nil
}

// VerifyIdentity checks both the unauthenticated version document and the
// authenticated session identity. It never retains the token echoed by OWU.
func (c *Client) VerifyIdentity(ctx context.Context) (Identity, error) {
	var version struct {
		Version      string `json:"version"`
		DeploymentID string `json:"deployment_id"`
	}
	if err := c.getJSON(ctx, "/api/version", false, &version); err != nil {
		return Identity{}, err
	}
	actualVersion := strings.TrimPrefix(strings.TrimSpace(version.Version), "v")
	if actualVersion != c.expectedVersion {
		return Identity{}, fmt.Errorf("Open WebUI version mismatch: expected %s", c.expectedVersion)
	}
	if strings.TrimSpace(version.DeploymentID) == "" {
		return Identity{}, errors.New("Open WebUI version response is missing deployment identity")
	}

	var account struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	}
	if err := c.getJSON(ctx, "/api/v1/auths/", true, &account); err != nil {
		return Identity{}, err
	}
	if strings.TrimSpace(account.ID) == "" {
		return Identity{}, errors.New("Open WebUI identity response is missing account id")
	}
	siteID := deriveSiteID(c.baseURL, version.DeploymentID)
	if c.expectedSiteID != "" && siteID != c.expectedSiteID {
		return Identity{}, errors.New("Open WebUI deployment identity mismatch")
	}
	if c.expectedAccountID != "" && account.ID != c.expectedAccountID {
		return Identity{}, errors.New("Open WebUI account identity mismatch")
	}
	return Identity{
		SiteID: siteID, DeploymentID: version.DeploymentID,
		AccountID: account.ID, Version: actualVersion,
		Email: account.Email, Name: account.Name, Role: account.Role,
	}, nil
}

func (c *Client) Create(ctx context.Context, request CreateRequest) (CreateReceipt, error) {
	if strings.TrimSpace(request.OperationID) == "" {
		return CreateReceipt{}, errors.New("operation id is required")
	}
	body, err := requestBody(request.Snapshot, request.OperationID, true)
	if err != nil {
		return CreateReceipt{}, err
	}
	raw, err := c.writeJSON(ctx, "/api/v1/chats/new", "create", body)
	if err != nil {
		return CreateReceipt{}, err
	}
	snapshot, err := decodeSnapshot(raw)
	if err != nil {
		return CreateReceipt{}, &OutcomeUnknownError{Operation: "create", Cause: "invalid receipt"}
	}
	if snapshot.ChatID == "" || (c.expectedAccountID != "" && snapshot.OwnerID != c.expectedAccountID) {
		return CreateReceipt{}, &OutcomeUnknownError{Operation: "create", Cause: "untrusted receipt identity"}
	}
	if !hasOperationMarker(snapshot.RawChat, request.OperationID, true) {
		return CreateReceipt{}, &OutcomeUnknownError{Operation: "create", Cause: "operation marker missing from receipt"}
	}
	return CreateReceipt{TargetID: snapshot.ChatID, Snapshot: snapshot}, nil
}

func (c *Client) Read(ctx context.Context, targetID string) (Snapshot, error) {
	path, err := chatPath(targetID)
	if err != nil {
		return Snapshot{}, err
	}
	raw, err := c.readJSON(ctx, path, true)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, err := decodeSnapshot(raw)
	if err != nil {
		return Snapshot{}, fmt.Errorf("Open WebUI read returned an invalid chat: %w", err)
	}
	if snapshot.ChatID != targetID {
		return Snapshot{}, errors.New("Open WebUI read returned a different chat id")
	}
	if c.expectedAccountID != "" && snapshot.OwnerID != c.expectedAccountID {
		return Snapshot{}, errors.New("Open WebUI read returned a different account owner")
	}
	return snapshot, nil
}

func (c *Client) Update(ctx context.Context, targetID string, request UpdateRequest) (UpdateReceipt, error) {
	path, err := chatPath(targetID)
	if err != nil {
		return UpdateReceipt{}, err
	}
	if strings.TrimSpace(request.OperationID) == "" {
		return UpdateReceipt{}, errors.New("operation id is required")
	}
	body, err := requestBody(request.Snapshot, request.OperationID, false)
	if err != nil {
		return UpdateReceipt{}, err
	}
	raw, err := c.writeJSON(ctx, path, "update", body)
	if err != nil {
		return UpdateReceipt{}, err
	}
	snapshot, err := decodeSnapshot(raw)
	if err != nil {
		return UpdateReceipt{}, &OutcomeUnknownError{Operation: "update", Cause: "invalid receipt"}
	}
	if snapshot.ChatID != targetID || (c.expectedAccountID != "" && snapshot.OwnerID != c.expectedAccountID) {
		return UpdateReceipt{}, &OutcomeUnknownError{Operation: "update", Cause: "untrusted receipt identity"}
	}
	if !hasOperationMarker(snapshot.RawChat, request.OperationID, false) {
		return UpdateReceipt{}, &OutcomeUnknownError{Operation: "update", Cause: "operation marker missing from receipt"}
	}
	return UpdateReceipt{Snapshot: snapshot}, nil
}

func chatPath(targetID string) (string, error) {
	if targetID == "" || strings.ContainsAny(targetID, "/?#\\\r\n") {
		return "", errors.New("target chat id is invalid")
	}
	return "/api/v1/chats/" + url.PathEscape(targetID), nil
}

func (c *Client) endpoint(path string) string {
	copy := *c.baseURL
	copy.Path = strings.TrimRight(copy.Path, "/") + path
	return copy.String()
}

func (c *Client) getJSON(ctx context.Context, path string, authenticated bool, target any) error {
	raw, err := c.readJSON(ctx, path, authenticated)
	if err != nil {
		return err
	}
	if err := decodeSingleJSON(raw, target); err != nil {
		return errors.New("Open WebUI returned malformed JSON")
	}
	return nil
}

func (c *Client) readJSON(ctx context.Context, path string, authenticated bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(path), nil)
	if err != nil {
		return nil, errors.New("could not build Open WebUI request")
	}
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, &HTTPError{Operation: "read"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{Operation: "read", StatusCode: resp.StatusCode}
	}
	return readBounded(resp.Body, c.maxResponseBytes)
}

func (c *Client) writeJSON(ctx context.Context, path, operation string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(path), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("could not build Open WebUI write request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, &OutcomeUnknownError{Operation: operation, Cause: "transport failure"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// v0.11.3 commits before publishing its update/create event. A 400 or
		// 5xx response can therefore arrive after the write was stored.
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestTimeout ||
			resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, &OutcomeUnknownError{Operation: operation, Cause: "ambiguous HTTP response"}
		}
		return nil, &HTTPError{Operation: operation, StatusCode: resp.StatusCode}
	}
	raw, err := readBounded(resp.Body, c.maxResponseBytes)
	if err != nil {
		return nil, &OutcomeUnknownError{Operation: operation, Cause: "incomplete receipt"}
	}
	return raw, nil
}

func deriveSiteID(base *url.URL, deploymentID string) string {
	canonical := strings.ToLower(base.Scheme) + "://" + strings.ToLower(base.Host) + strings.TrimRight(base.EscapedPath(), "/")
	digest := sha256.Sum256([]byte(canonical + "\x00" + deploymentID))
	return fmt.Sprintf("sha256:%x", digest[:])
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(reader, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("Open WebUI response could not be read")
	}
	if int64(len(body)) > limit {
		return nil, errors.New("Open WebUI response exceeds the configured limit")
	}
	return body, nil
}

func decodeSingleJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}
