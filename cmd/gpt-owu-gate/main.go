package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/config"
	"github.com/ChenM0M/gpt-owu-bridge/internal/diff"
	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
	"github.com/ChenM0M/gpt-owu-bridge/internal/httpserver"
	"github.com/ChenM0M/gpt-owu-bridge/internal/source/chatgpt"
)

func main() {
	if err := run(os.Args[1:], os.Environ(), os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args, environ []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "healthcheck":
		cfg, err := config.Load(args[1:], environ)
		if err != nil {
			return err
		}
		client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Get("http://" + cfg.Listen + "/health/live")
		if err != nil {
			return errors.New("health endpoint unavailable")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return errors.New("health endpoint unhealthy")
		}
		return nil
	case "serve":
		return runServe(args[1:], environ, stderr)
	case "config":
		if len(args) < 2 || args[1] != "check" {
			return errors.New("usage: gpt-owu-gate config check [flags]")
		}
		return runConfigCheck(args[2:], environ, stdout)
	case "preview":
		return runPreview(args[1:], stdout)
	case "sync":
		return runSync(args[1:], environ, stdout)
	case "help", "-h", "--help":
		_, _ = fmt.Fprintln(stdout, usageText())
		return nil
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New(usageText())
}

func usageText() string {
	return strings.TrimSpace(`usage:
  gpt-owu-gate serve [config flags]
  gpt-owu-gate config check [config flags]
  gpt-owu-gate preview --html FILE --snapshot-out FILE [preview flags]
  gpt-owu-gate sync init|preview|apply|status|pending|recover [action flags] -- [config flags]

serve exposes local health endpoints only. preview is offline and read-only.
sync is a local administrator command; run sync help for details.`)
}

func runConfigCheck(args, environ []string, stdout io.Writer) error {
	cfg, err := config.Load(args, environ)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	b, err := cfg.RedactedJSON()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(b))
	return err
}

func runServe(args, environ []string, stderr io.Writer) error {
	cfg, err := config.Load(args, environ)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	level := new(slog.LevelVar)
	switch cfg.LogLevel {
	case "debug":
		level.Set(slog.LevelDebug)
	case "warn":
		level.Set(slog.LevelWarn)
	case "error":
		level.Set(slog.LevelError)
	default:
		level.Set(slog.LevelInfo)
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return httpserver.Run(ctx, cfg, logger)
}

type previewOutput struct {
	EvidenceScope string          `json:"evidence_scope"`
	Source        sourceSummary   `json:"source"`
	Plan          domain.SyncPlan `json:"plan"`
}

type sourceSummary struct {
	Title            string                   `json:"title,omitempty"`
	IdentityEvidence domain.EvidenceLevel     `json:"identity_evidence"`
	MessageCount     int                      `json:"message_count"`
	Roles            map[string]int           `json:"roles"`
	CoverageStatus   string                   `json:"coverage_status"`
	Unsupported      []domain.UnsupportedItem `json:"unsupported,omitempty"`
	BusinessHash     string                   `json:"business_hash"`
	SnapshotOutput   string                   `json:"snapshot_output"`
}

func runPreview(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	htmlPath := fs.String("html", "", "synthetic or redacted share HTML")
	snapshotOut := fs.String("snapshot-out", "", "private normalized snapshot output")
	previousPath := fs.String("previous", "", "previous normalized source snapshot")
	baselinePath := fs.String("target-baseline", "", "offline target baseline JSON")
	targetPath := fs.String("target-current", "", "offline current target JSON")
	titlePolicy := fs.String("title-policy", string(domain.TitleFollowSource), "follow_source or target_override")
	stableIDs := fs.Bool("stable-source-ids", false, "declare IDs stable for this synthetic/offline comparison")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("invalid preview flags: %w", err)
	}
	if fs.NArg() != 0 || *htmlPath == "" || *snapshotOut == "" {
		return errors.New("preview requires --html and --snapshot-out, with no positional arguments")
	}
	if (*baselinePath == "") != (*targetPath == "") {
		return errors.New("--target-baseline and --target-current must be supplied together")
	}
	if err := validatePreviewPaths(*htmlPath, *snapshotOut, *previousPath, *baselinePath, *targetPath); err != nil {
		return err
	}

	var previous *domain.SourceSnapshot
	if *previousPath != "" {
		value := new(domain.SourceSnapshot)
		if err := readJSON(*previousPath, value); err != nil {
			return fmt.Errorf("read previous source snapshot: %w", err)
		}
		previous = value
	}
	var baseline, target *domain.TargetSnapshot
	if *baselinePath != "" {
		baseline = new(domain.TargetSnapshot)
		target = new(domain.TargetSnapshot)
		if err := readJSON(*baselinePath, baseline); err != nil {
			return fmt.Errorf("read target baseline: %w", err)
		}
		if err := readJSON(*targetPath, target); err != nil {
			return fmt.Errorf("read current target: %w", err)
		}
	}
	html, err := readFileLimited(*htmlPath, int64(chatgpt.DefaultLimits().MaxHTMLBytes))
	if err != nil {
		return fmt.Errorf("preview HTML: %w", err)
	}
	snapshot, err := chatgpt.ParseHTML(html, chatgpt.ParseOptions{FetchedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	plan, err := diff.Preview(previous, snapshot, baseline, target, diff.Options{
		Now: time.Now().UTC(), TitlePolicy: domain.TitlePolicy(*titlePolicy), StableSourceIDs: *stableIDs,
	})
	if err != nil {
		return err
	}
	roles := make(map[string]int)
	for _, message := range snapshot.Messages {
		roles[message.Role]++
	}
	output := previewOutput{
		EvidenceScope: "offline synthetic/redacted HTML parse and read-only diff; no OWU or ChatGPT network call",
		Source: sourceSummary{
			Title: snapshot.Title, IdentityEvidence: snapshot.Identity.Evidence,
			MessageCount: len(snapshot.Messages), Roles: roles, CoverageStatus: snapshot.Coverage.Status,
			Unsupported: snapshot.Coverage.Unsupported, BusinessHash: snapshot.BusinessHash,
			SnapshotOutput: filepath.Clean(*snapshotOut),
		},
		Plan: plan,
	}
	encodedOutput, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("encode preview output: %w", err)
	}
	encodedOutput = append(encodedOutput, '\n')
	if err := writePrivateJSON(*snapshotOut, snapshot); err != nil {
		return err
	}
	_, err = stdout.Write(encodedOutput)
	return err
}

func validatePreviewPaths(htmlPath, snapshotOut, previousPath, baselinePath, targetPath string) error {
	inputs := []struct {
		flag       string
		path       string
		allowAlias bool
	}{
		{flag: "--html", path: htmlPath},
		{flag: "--previous", path: previousPath, allowAlias: true},
		{flag: "--target-baseline", path: baselinePath},
		{flag: "--target-current", path: targetPath},
	}
	for _, input := range inputs {
		if input.path == "" {
			continue
		}
		aliases, err := pathsAlias(snapshotOut, input.path)
		if err != nil {
			return fmt.Errorf("check preview paths: %w", err)
		}
		if aliases && !input.allowAlias {
			return fmt.Errorf("--snapshot-out must not refer to the same file as %s", input.flag)
		}
	}
	return nil
}

func pathsAlias(left, right string) (bool, error) {
	leftAbsolute, err := filepath.Abs(left)
	if err != nil {
		return false, errors.New("snapshot output path cannot be resolved")
	}
	rightAbsolute, err := filepath.Abs(right)
	if err != nil {
		return false, errors.New("preview input path cannot be resolved")
	}
	if filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute) {
		return true, nil
	}
	leftInfo, err := os.Stat(left)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, errors.New("snapshot output path cannot be inspected")
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, errors.New("preview input path cannot be inspected")
	}
	return os.SameFile(leftInfo, rightInfo), nil
}

func readJSON(path string, value any) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("file is not readable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > 16<<20 {
		return errors.New("JSON input exceeds size limit")
	}
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("invalid JSON input")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("JSON input contains trailing data")
	}
	return nil
}

func readFileLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return io.ReadAll(io.LimitReader(f, limit+1))
}

func writePrivateJSON(path string, value any) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("snapshot output directory cannot be created")
	}
	temporary, err := os.CreateTemp(directory, ".snapshot-*.tmp")
	if err != nil {
		return errors.New("snapshot output cannot be created")
	}
	temporaryName := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.New("snapshot output permissions cannot be set")
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return errors.New("snapshot output cannot be encoded")
	}
	if err := temporary.Sync(); err != nil {
		return errors.New("snapshot output cannot be flushed")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("snapshot output cannot be closed")
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return errors.New("snapshot output cannot be installed")
	}
	keep = true
	return nil
}
