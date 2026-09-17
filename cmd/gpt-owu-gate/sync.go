package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/config"
	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
	"github.com/ChenM0M/gpt-owu-bridge/internal/owu"
	"github.com/ChenM0M/gpt-owu-bridge/internal/source/chatgpt"
	"github.com/ChenM0M/gpt-owu-bridge/internal/storage"
	"github.com/ChenM0M/gpt-owu-bridge/internal/syncer"
)

const syncEvidenceScope = "local administrator CLI; OWU v0.11.3 source contract; real OWU and ChatGPT end-to-end acceptance pending"

func runSync(args, environ []string, stdout io.Writer) error {
	flags, err := parseSyncFlags(args)
	if err != nil {
		return err
	}
	if flags.action == "help" {
		_, err = io.WriteString(stdout, syncUsage+"\n")
		return err
	}
	cfg, err := config.Load(flags.configArgs, environ)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	databasePath := filepath.Join(cfg.DataDir, storage.DefaultDatabaseFilename)
	if flags.action == "init" {
		client, err := syncClient(cfg)
		if err != nil {
			return err
		}
		identity, err := client.VerifyIdentity(ctx)
		if err != nil {
			return err
		}
		store, installation, err := storage.Initialize(ctx, databasePath, storage.TargetIdentity{OWUSiteID: identity.SiteID, OWUAccountID: identity.AccountID})
		if err != nil {
			return err
		}
		defer store.Close()
		return writeSyncOutput(stdout, map[string]any{"evidence_scope": syncEvidenceScope, "status": "initialized", "installation": installation})
	}
	store, installation, err := storage.Open(ctx, databasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	if flags.action == "pending" {
		records, err := store.ListUnresolvedOperations(ctx)
		if err != nil {
			return err
		}
		operations := make([]domain.Operation, 0, len(records))
		for _, record := range records {
			operations = append(operations, record.Operation)
		}
		return writeSyncOutput(stdout, map[string]any{"evidence_scope": syncEvidenceScope, "operations": operations})
	}
	// Status has no downstream capability and remains usable during an outage.
	var target syncer.TargetClient
	if flags.action != "status" {
		if cfg.OWUBaseURL == "" || !cfg.HasOWUToken() {
			return errors.New("sync requires configured OWU base URL and token")
		}
		client, err := owu.New(owu.Config{BaseURL: cfg.OWUBaseURL, Token: cfg.OWUToken(), ExpectedVersion: owu.SupportedVersion,
			ExpectedSiteID: installation.OWUSiteID, ExpectedAccountID: installation.OWUAccountID, Timeout: 15 * time.Second})
		if err != nil {
			return err
		}
		target = client
	}
	service := syncer.New(store, target, installation, syncer.Options{})
	switch flags.action {
	case "preview":
		html, err := readFileLimited(flags.html, int64(chatgpt.DefaultLimits().MaxHTMLBytes))
		if err != nil {
			return errors.New("sync HTML cannot be read within the size limit")
		}
		snapshot, err := chatgpt.ParseHTML(html, chatgpt.ParseOptions{FetchedAt: time.Now().UTC()})
		if err != nil {
			return err
		}
		plan, err := service.Preview(ctx, installation.OwnerID, snapshot, flags.binding)
		if err != nil {
			return err
		}
		roles := make(map[string]int)
		for _, message := range snapshot.Messages {
			roles[message.Role]++
		}
		return writeSyncOutput(stdout, map[string]any{"evidence_scope": syncEvidenceScope, "plan": plan, "source": sourceSummary{Title: snapshot.Title, IdentityEvidence: snapshot.Identity.Evidence, MessageCount: len(snapshot.Messages), Roles: roles, CoverageStatus: snapshot.Coverage.Status, Unsupported: snapshot.Coverage.Unsupported, BusinessHash: snapshot.BusinessHash}})
	case "apply", "status", "recover":
		var operation domain.Operation
		switch flags.action {
		case "apply":
			operation, err = service.Apply(ctx, installation.OwnerID, flags.plan, flags.confirm)
		case "status":
			operation, err = service.Status(ctx, installation.OwnerID, flags.operation)
		case "recover":
			operation, err = service.Recover(ctx, installation.OwnerID, flags.operation)
		}
		// Persisted failure/reconciliation state is useful even if a downstream
		// call failed. Never print raw requests, payloads, or credentials.
		if operation.ID != "" {
			if outputErr := writeSyncOutput(stdout, map[string]any{"evidence_scope": syncEvidenceScope, "operation": operation}); outputErr != nil {
				return outputErr
			}
		}
		return err
	}
	return errors.New("unsupported sync action")
}

func syncClient(cfg config.Config) (*owu.Client, error) {
	if cfg.OWUBaseURL == "" || !cfg.HasOWUToken() {
		return nil, errors.New("sync requires configured OWU base URL and token")
	}
	return owu.New(owu.Config{BaseURL: cfg.OWUBaseURL, Token: cfg.OWUToken(), ExpectedVersion: "0.11.3", Timeout: 15 * time.Second})
}

func writeSyncOutput(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
