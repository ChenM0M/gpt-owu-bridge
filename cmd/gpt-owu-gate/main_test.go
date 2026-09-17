package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
)

func TestPreviewCLIReadsInputsBeforeInstallingSnapshot(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "testdata", "synthetic-share.html")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	changedHTML := bytes.Replace(fixture, []byte("M1 合成分享：格式核对"), []byte("M1 合成分享：修补核对"), 1)
	if bytes.Equal(changedHTML, fixture) {
		t.Fatal("fixture title was not replaced")
	}
	temporary := t.TempDir()
	changedPath := filepath.Join(temporary, "changed.html")
	if err := os.WriteFile(changedPath, changedHTML, 0o600); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(temporary, "old.json")
	if _, err := runPreviewCLI(fixturePath, oldPath); err != nil {
		t.Fatal(err)
	}
	oldBytes, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("separate output detects title change", func(t *testing.T) {
		output, err := runPreviewCLI(changedPath, filepath.Join(temporary, "new.json"),
			"--previous", oldPath, "--stable-source-ids")
		if err != nil {
			t.Fatal(err)
		}
		if output.Plan.Status != "ready" || output.Plan.Diff.TitleChange != "sync_source" {
			t.Fatalf("title change was lost: %#v", output.Plan)
		}
	})

	t.Run("same previous and output path rolls over after comparison", func(t *testing.T) {
		inPlace := filepath.Join(temporary, "in-place.json")
		if err := os.WriteFile(inPlace, oldBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := runPreviewCLI(changedPath, inPlace,
			"--previous", inPlace, "--stable-source-ids")
		if err != nil {
			t.Fatal(err)
		}
		if output.Plan.Status != "ready" || output.Plan.Diff.TitleChange != "sync_source" {
			t.Fatalf("in-place comparison used the new snapshot as its baseline: %#v", output.Plan)
		}
		var installed domain.SourceSnapshot
		if err := readJSON(inPlace, &installed); err != nil {
			t.Fatal(err)
		}
		if installed.Title != "M1 合成分享：修补核对" {
			t.Fatalf("new snapshot was not installed: title=%q", installed.Title)
		}
	})

	t.Run("invalid previous preserves existing output", func(t *testing.T) {
		badPrevious := filepath.Join(temporary, "bad-previous.json")
		if err := os.WriteFile(badPrevious, []byte("not JSON"), 0o600); err != nil {
			t.Fatal(err)
		}
		existing := filepath.Join(temporary, "existing.json")
		const sentinel = "EXISTING_OUTPUT_SENTINEL"
		if err := os.WriteFile(existing, []byte(sentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runPreviewCLI(changedPath, existing, "--previous", badPrevious); err == nil {
			t.Fatal("invalid previous snapshot should fail")
		}
		contents, err := os.ReadFile(existing)
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != sentinel {
			t.Fatalf("failed preview changed existing output: %q", contents)
		}
	})

	t.Run("parse failure preserves existing output", func(t *testing.T) {
		invalidHTML := filepath.Join(temporary, "invalid.html")
		if err := os.WriteFile(invalidHTML, []byte("<html>no supported table</html>"), 0o600); err != nil {
			t.Fatal(err)
		}
		existing := filepath.Join(temporary, "parse-failure-output.json")
		const sentinel = "PARSE_FAILURE_SENTINEL"
		if err := os.WriteFile(existing, []byte(sentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runPreviewCLI(invalidHTML, existing); err == nil {
			t.Fatal("unsupported HTML should fail")
		}
		assertFileContents(t, existing, sentinel)
	})

	t.Run("plan failure preserves existing output", func(t *testing.T) {
		existing := filepath.Join(temporary, "plan-failure-output.json")
		const sentinel = "PLAN_FAILURE_SENTINEL"
		if err := os.WriteFile(existing, []byte(sentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := runPreviewCLI(changedPath, existing,
			"--previous", oldPath, "--stable-source-ids", "--title-policy", "invalid"); err == nil {
			t.Fatal("invalid title policy should fail")
		}
		assertFileContents(t, existing, sentinel)
	})
}

func TestPreviewCLIPathAliasPolicy(t *testing.T) {
	temporary := t.TempDir()
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "synthetic-share.html"))
	if err != nil {
		t.Fatal(err)
	}
	htmlPath := filepath.Join(temporary, "input.html")
	if err := os.WriteFile(htmlPath, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	htmlAlias := filepath.Join(temporary, "input-alias")
	if err := os.Link(htmlPath, htmlAlias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := runPreviewCLI(htmlPath, htmlAlias); err == nil || !strings.Contains(err.Error(), "same file as --html") {
		t.Fatalf("HTML/output alias should be rejected, got %v", err)
	}
	unchanged, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unchanged, fixture) {
		t.Fatal("alias rejection changed the HTML input")
	}

	previous := filepath.Join(temporary, "previous.json")
	if _, err := runPreviewCLI(htmlPath, previous); err != nil {
		t.Fatal(err)
	}
	outputAlias := filepath.Join(temporary, "previous-alias.json")
	if err := os.Link(previous, outputAlias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	changedHTML := bytes.Replace(fixture, []byte("M1 合成分享：格式核对"), []byte("M1 合成分享：别名核对"), 1)
	changedPath := filepath.Join(temporary, "changed.html")
	if err := os.WriteFile(changedPath, changedHTML, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := runPreviewCLI(changedPath, outputAlias,
		"--previous", previous, "--stable-source-ids")
	if err != nil {
		t.Fatal(err)
	}
	if output.Plan.Status != "ready" || output.Plan.Diff.TitleChange != "sync_source" {
		t.Fatalf("previous/output alias was not compared before rollover: %#v", output.Plan)
	}
}

func runPreviewCLI(htmlPath, snapshotOut string, extra ...string) (previewOutput, error) {
	args := []string{"preview", "--html", htmlPath, "--snapshot-out", snapshotOut}
	args = append(args, extra...)
	var stdout bytes.Buffer
	err := run(args, nil, &stdout, &bytes.Buffer{})
	if err != nil {
		return previewOutput{}, err
	}
	var output previewOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return previewOutput{}, err
	}
	return output, nil
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != want {
		t.Fatalf("file %q changed: got %q want %q", path, contents, want)
	}
}
