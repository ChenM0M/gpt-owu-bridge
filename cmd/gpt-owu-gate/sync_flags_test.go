package main

import (
	"strings"
	"testing"
)

func TestSyncCommandBoundary(t *testing.T) {
	for _, args := range [][]string{
		{"apply", "--plan", "p"},
		{"apply", "--plan", "p", "--confirm=false"},
		{"apply", "--plan", "p", "--confirm", "--target", "private-chat"},
		{"preview", "--html", "share.html", "--stable-source-ids"},
		{"preview", "--html", "share.html", "--principal", "other"},
		{"recover", "--operation", "op", "--target", "arbitrary"},
		{"init", "--token", "canary-token-do-not-print"},
		{"init", "--canary-token-do-not-print"},
	} {
		_, err := parseSyncFlags(args)
		if err == nil {
			t.Fatalf("accepted forbidden flags %v", args)
		}
		if strings.Contains(err.Error(), "canary-token") {
			t.Fatal("secret in flag error")
		}
	}
	f, err := parseSyncFlags([]string{"apply", "--plan", "p", "--confirm", "--", "--data-dir", "/tmp/private"})
	if err != nil || !f.confirm || f.plan != "p" || len(f.configArgs) != 2 {
		t.Fatalf("valid command: %#v %v", f, err)
	}
}
