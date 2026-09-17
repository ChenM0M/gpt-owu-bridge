package main

import (
	"errors"
	"flag"
	"io"
	"strings"
)

const syncUsage = `usage:
  gpt-owu-gate sync init -- [config flags]
  gpt-owu-gate sync preview --html FILE [--binding ID] -- [config flags]
  gpt-owu-gate sync preview --url HTTPS_SHARE_URL [--binding ID] -- [config flags]
  gpt-owu-gate sync apply --plan ID --confirm -- [config flags]
  gpt-owu-gate sync status --operation ID -- [config flags]
  gpt-owu-gate sync pending -- [config flags]
  gpt-owu-gate sync recover --operation ID -- [config flags]

Configuration flags follow the -- separator, for example --data-dir DIR.
OWU base URL and token use existing configuration; no token CLI flag exists.
init, preview, apply and recover contact the configured OWU site.
status and pending only read local state. init explicitly creates a new installation.
Only use an authorized dedicated test environment pending real acceptance.
Changed-source automatic matching remains disabled pending lifecycle evidence.`

type syncFlags struct {
	action     string
	html       string
	shareURL   string
	binding    string
	plan       string
	operation  string
	confirm    bool
	configArgs []string
}

func parseSyncFlags(args []string) (syncFlags, error) {
	var out syncFlags
	if len(args) == 0 {
		return out, errors.New(syncUsage)
	}
	out.action = args[0]
	if out.action == "help" || out.action == "--help" || out.action == "-h" {
		if len(args) != 1 {
			return out, errors.New(syncUsage)
		}
		out.action = "help"
		return out, nil
	}
	actionArgs := args[1:]
	for i, v := range actionArgs {
		if v == "--" {
			out.configArgs = actionArgs[i+1:]
			actionArgs = actionArgs[:i]
			break
		}
	}
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch out.action {
	case "init", "pending":
	case "preview":
		fs.StringVar(&out.html, "html", "", "local share HTML")
		fs.StringVar(&out.shareURL, "url", "", "canonical public ChatGPT share URL")
		fs.StringVar(&out.binding, "binding", "", "service binding identifier")
	case "apply":
		fs.StringVar(&out.plan, "plan", "", "persisted plan identifier")
		fs.BoolVar(&out.confirm, "confirm", false, "confirm this persisted plan")
	case "status", "recover":
		fs.StringVar(&out.operation, "operation", "", "persisted operation identifier")
	default:
		return out, errors.New(syncUsage)
	}
	// Deliberately do not interpolate flag errors: unknown flags or their values
	// can contain accidentally pasted secrets.
	if fs.Parse(actionArgs) != nil || fs.NArg() != 0 {
		return out, errors.New("invalid sync flags; run sync help")
	}
	if out.action == "preview" && ((strings.TrimSpace(out.html) == "") == (strings.TrimSpace(out.shareURL) == "")) {
		return out, errors.New("sync preview requires exactly one of --html or --url")
	}
	if out.action == "apply" && (strings.TrimSpace(out.plan) == "" || !out.confirm) {
		return out, errors.New("sync apply requires --plan and --confirm for the reviewed plan")
	}
	if (out.action == "status" || out.action == "recover") && strings.TrimSpace(out.operation) == "" {
		return out, errors.New("sync status/recover requires --operation")
	}
	return out, nil
}
