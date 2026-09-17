// Package config provides strict, source-aware configuration loading. Values
// are merged in the order defaults < YAML < environment < explicit CLI flags.
package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const maxSecretBytes = 64 << 10

type Config struct {
	Listen          string
	LogLevel        string
	DataDir         string
	PublicURL       string
	OWUBaseURL      string
	OWUTokenFile    string
	ShutdownTimeout time.Duration
	owuToken        string
}

type rawConfig struct {
	Listen          string
	LogLevel        string
	DataDir         string
	PublicURL       string
	OWUBaseURL      string
	OWUTokenFile    string
	OWUToken        string
	ShutdownTimeout string
}

func defaults() rawConfig {
	return rawConfig{
		Listen:          "127.0.0.1:8080",
		LogLevel:        "info",
		DataDir:         "./data",
		ShutdownTimeout: "10s",
	}
}

type optionalString struct {
	set   bool
	value string
}

func (v *optionalString) String() string { return v.value }
func (v *optionalString) Set(s string) error {
	v.set = true
	v.value = s
	return nil
}

// Load parses common service flags, loads an optional YAML file, merges
// recognized GATE_ variables, validates the result, and resolves a configured
// token without ever returning it in an error.
func Load(args, environ []string) (Config, error) {
	var cli struct {
		configPath optionalString
		listen     optionalString
		logLevel   optionalString
		dataDir    optionalString
		publicURL  optionalString
		owuBaseURL optionalString
		shutdown   optionalString
	}
	fs := flag.NewFlagSet("gpt-owu-gate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(&cli.configPath, "config", "YAML configuration file")
	fs.Var(&cli.listen, "listen", "loopback listen address")
	fs.Var(&cli.logLevel, "log-level", "debug, info, warn, or error")
	fs.Var(&cli.dataDir, "data-dir", "runtime data directory")
	fs.Var(&cli.publicURL, "public-url", "public HTTPS URL")
	fs.Var(&cli.owuBaseURL, "owu-base-url", "fixed Open WebUI base URL")
	fs.Var(&cli.shutdown, "shutdown-timeout", "graceful shutdown timeout")
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("invalid command flags: %w", err)
	}
	if fs.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional argument")
	}

	env, err := parseEnv(environ)
	if err != nil {
		return Config{}, err
	}

	raw := defaults()
	configPath := env["GATE_CONFIG"]
	if cli.configPath.set {
		configPath = cli.configPath.value
	}
	if configPath != "" {
		fileValues, err := parseYAMLFile(configPath)
		if err != nil {
			return Config{}, err
		}
		applyMap(&raw, fileValues)
	}
	applyEnv(&raw, env)
	applyCLI(&raw, cli.listen, cli.logLevel, cli.dataDir, cli.publicURL, cli.owuBaseURL, cli.shutdown)

	cfg, err := finalize(raw)
	if err != nil {
		return Config{}, err
	}
	return cfg, nil
}

var allowedEnv = map[string]struct{}{
	"GATE_CONFIG": {}, "GATE_LISTEN": {}, "GATE_LOG_LEVEL": {},
	"GATE_DATA_DIR": {}, "GATE_PUBLIC_URL": {}, "GATE_OWU_BASE_URL": {},
	"GATE_OWU_TOKEN": {}, "GATE_OWU_TOKEN_FILE": {},
	"GATE_SHUTDOWN_TIMEOUT": {},
}

func parseEnv(environ []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, pair := range environ {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(name, "GATE_") {
			continue
		}
		if _, ok := allowedEnv[name]; !ok {
			return nil, fmt.Errorf("unknown GATE_ environment variable %q", name)
		}
		result[name] = value
	}
	return result, nil
}

var yamlKeys = map[string]struct{}{
	"listen": {}, "log_level": {}, "data_dir": {}, "public_url": {},
	"owu_base_url": {}, "owu_token_file": {}, "shutdown_timeout": {},
}

func parseYAMLFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("configuration file is not readable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > 1<<20 {
		return nil, fmt.Errorf("configuration file exceeds size limit")
	}

	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(scanner.Text(), " ") || strings.HasPrefix(scanner.Text(), "\t") {
			return nil, fmt.Errorf("configuration line %d: nested YAML is not supported", lineNo)
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("configuration line %d: expected key: value", lineNo)
		}
		key = strings.TrimSpace(key)
		if _, ok := yamlKeys[key]; !ok {
			return nil, fmt.Errorf("configuration line %d: unknown key %q", lineNo, key)
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("configuration line %d: duplicate key %q", lineNo, key)
		}
		parsed, err := parseScalar(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("configuration line %d: invalid scalar", lineNo)
		}
		values[key] = parsed
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read configuration file: %w", err)
	}
	return values, nil
}

func parseScalar(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "\"") {
		return strconv.Unquote(value)
	}
	if strings.HasPrefix(value, "'") {
		if len(value) < 2 || !strings.HasSuffix(value, "'") {
			return "", errors.New("unterminated single-quoted value")
		}
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), nil
	}
	if before, _, found := strings.Cut(value, " #"); found {
		value = before
	}
	return strings.TrimSpace(value), nil
}

func applyMap(raw *rawConfig, values map[string]string) {
	for key, value := range values {
		switch key {
		case "listen":
			raw.Listen = value
		case "log_level":
			raw.LogLevel = value
		case "data_dir":
			raw.DataDir = value
		case "public_url":
			raw.PublicURL = value
		case "owu_base_url":
			raw.OWUBaseURL = value
		case "owu_token_file":
			raw.OWUTokenFile = value
		case "shutdown_timeout":
			raw.ShutdownTimeout = value
		}
	}
}

func applyEnv(raw *rawConfig, env map[string]string) {
	if value, ok := env["GATE_LISTEN"]; ok {
		raw.Listen = value
	}
	if value, ok := env["GATE_LOG_LEVEL"]; ok {
		raw.LogLevel = value
	}
	if value, ok := env["GATE_DATA_DIR"]; ok {
		raw.DataDir = value
	}
	if value, ok := env["GATE_PUBLIC_URL"]; ok {
		raw.PublicURL = value
	}
	if value, ok := env["GATE_OWU_BASE_URL"]; ok {
		raw.OWUBaseURL = value
	}
	if value, ok := env["GATE_OWU_TOKEN_FILE"]; ok {
		raw.OWUTokenFile = value
	}
	if value, ok := env["GATE_OWU_TOKEN"]; ok {
		raw.OWUToken = value
	}
	if value, ok := env["GATE_SHUTDOWN_TIMEOUT"]; ok {
		raw.ShutdownTimeout = value
	}
}

func applyCLI(raw *rawConfig, listen, level, dataDir, publicURL, owuBaseURL, shutdown optionalString) {
	if listen.set {
		raw.Listen = listen.value
	}
	if level.set {
		raw.LogLevel = level.value
	}
	if dataDir.set {
		raw.DataDir = dataDir.value
	}
	if publicURL.set {
		raw.PublicURL = publicURL.value
	}
	if owuBaseURL.set {
		raw.OWUBaseURL = owuBaseURL.value
	}
	if shutdown.set {
		raw.ShutdownTimeout = shutdown.value
	}
}

func finalize(raw rawConfig) (Config, error) {
	if raw.OWUToken != "" && raw.OWUTokenFile != "" {
		return Config{}, errors.New("OWU token literal and token file cannot both be configured")
	}
	if err := validateLoopback(raw.Listen); err != nil {
		return Config{}, err
	}
	level := strings.ToLower(raw.LogLevel)
	switch level {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, errors.New("log_level must be debug, info, warn, or error")
	}
	if strings.TrimSpace(raw.DataDir) == "" {
		return Config{}, errors.New("data_dir must not be empty")
	}
	if err := validateURL("public_url", raw.PublicURL, true); err != nil {
		return Config{}, err
	}
	if err := validateURL("owu_base_url", raw.OWUBaseURL, false); err != nil {
		return Config{}, err
	}
	shutdown, err := time.ParseDuration(raw.ShutdownTimeout)
	if err != nil || shutdown <= 0 || shutdown > time.Minute {
		return Config{}, errors.New("shutdown_timeout must be greater than zero and at most 1m")
	}
	token := raw.OWUToken
	if raw.OWUTokenFile != "" {
		resolved, err := readSecret(raw.OWUTokenFile)
		if err != nil {
			return Config{}, err
		}
		token = resolved
	}
	if raw.OWUToken != "" && strings.TrimSpace(raw.OWUToken) == "" {
		return Config{}, errors.New("OWU token is empty")
	}
	return Config{
		Listen: raw.Listen, LogLevel: level, DataDir: filepath.Clean(raw.DataDir),
		PublicURL: raw.PublicURL, OWUBaseURL: strings.TrimRight(raw.OWUBaseURL, "/"),
		OWUTokenFile: raw.OWUTokenFile, ShutdownTimeout: shutdown, owuToken: token,
	}, nil
}

func validateLoopback(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("listen must be a host:port address")
	}
	if host == "localhost" {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return errors.New("listen must use an explicit loopback address")
	}
	return nil
}

func validateURL(name, value string, requireHTTPS bool) error {
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must be an absolute URL without credentials, query, or fragment", name)
	}
	if requireHTTPS && u.Scheme != "https" {
		return fmt.Errorf("%s must use https", name)
	}
	if !requireHTTPS && u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("%s must use http or https", name)
	}
	return nil
}

func readSecret(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("OWU token file is not readable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil {
		return "", errors.New("OWU token file could not be read")
	}
	if len(b) > maxSecretBytes {
		return "", errors.New("OWU token file is too large")
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", errors.New("OWU token file is empty")
	}
	return token, nil
}

func (c Config) HasOWUToken() bool { return c.owuToken != "" }

// OWUToken supplies the resolved secret exclusively to the fixed downstream
// client. Diagnostics must use RedactedJSON instead.
func (c Config) OWUToken() string { return c.owuToken }

// RedactedJSON is suitable for an operator-facing config check. It reports
// whether a secret is configured, never its value.
func (c Config) RedactedJSON() ([]byte, error) {
	return json.MarshalIndent(struct {
		Listen          string `json:"listen"`
		LogLevel        string `json:"log_level"`
		DataDir         string `json:"data_dir"`
		PublicURL       string `json:"public_url,omitempty"`
		OWUBaseURL      string `json:"owu_base_url,omitempty"`
		OWUTokenSet     bool   `json:"owu_token_set"`
		OWUTokenSource  string `json:"owu_token_source,omitempty"`
		ShutdownTimeout string `json:"shutdown_timeout"`
	}{
		Listen: c.Listen, LogLevel: c.LogLevel, DataDir: c.DataDir,
		PublicURL: c.PublicURL, OWUBaseURL: c.OWUBaseURL, OWUTokenSet: c.HasOWUToken(),
		OWUTokenSource: tokenSource(c), ShutdownTimeout: c.ShutdownTimeout.String(),
	}, "", "  ")
}

func tokenSource(c Config) string {
	if !c.HasOWUToken() {
		return ""
	}
	if c.OWUTokenFile != "" {
		return "file"
	}
	return "environment"
}

// EnsureDataDir proves that the directory is writable without leaving a data
// file behind. It is intentionally not used by the pure offline preview path.
func (c Config) EnsureDataDir() error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return errors.New("data directory cannot be created")
	}
	f, err := os.CreateTemp(c.DataDir, ".write-check-*")
	if err != nil {
		return errors.New("data directory is not writable")
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return errors.New("data directory write check failed")
	}
	if err := os.Remove(name); err != nil {
		return errors.New("data directory write check cleanup failed")
	}
	return nil
}
