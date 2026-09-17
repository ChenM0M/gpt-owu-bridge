package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const DefaultDatabaseFilename = "gate.db"

type Store struct {
	db           *sql.DB
	conn         *sql.Conn
	path         string
	installation Installation
	mu           sync.Mutex
	closeOnce    sync.Once
	closeErr     error
}

// Initialize creates a new database and fixes its installation, owner, OWU
// site, and OWU account identity. It never adopts an existing file.
func Initialize(ctx context.Context, databasePath string, target TargetIdentity) (_ *Store, _ Installation, err error) {
	if err := validateTargetIdentity(target); err != nil {
		return nil, Installation{}, err
	}
	abs, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, Installation{}, fmt.Errorf("resolve database path: %w", err)
	}
	if _, err := os.Lstat(abs); err == nil {
		return nil, Installation{}, ErrAlreadyInitialized
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, Installation{}, fmt.Errorf("inspect database path: %w", err)
	}
	for _, sidecar := range []string{abs + "-wal", abs + "-shm"} {
		if _, sidecarErr := os.Lstat(sidecar); sidecarErr == nil {
			return nil, Installation{}, fmt.Errorf("%w: SQLite sidecar exists without database", ErrAlreadyInitialized)
		} else if !errors.Is(sidecarErr, os.ErrNotExist) {
			return nil, Installation{}, fmt.Errorf("inspect SQLite sidecar: %w", sidecarErr)
		}
	}
	if err := ensurePrivateDirectory(filepath.Dir(abs)); err != nil {
		return nil, Installation{}, err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, Installation{}, ErrAlreadyInitialized
		}
		return nil, Installation{}, fmt.Errorf("create database: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(abs)
		return nil, Installation{}, fmt.Errorf("close new database file: %w", err)
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(abs)
			_ = os.Remove(abs + "-wal")
			_ = os.Remove(abs + "-shm")
		}
	}()
	s, err := openDatabase(ctx, abs, true)
	if err != nil {
		return nil, Installation{}, err
	}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()

	installationID, err := newID()
	if err != nil {
		return nil, Installation{}, err
	}
	ownerID, err := newID()
	if err != nil {
		return nil, Installation{}, err
	}
	now := time.Now().UTC()
	installation := Installation{
		ID:           installationID,
		OwnerID:      ownerID,
		OWUSiteID:    strings.TrimSpace(target.OWUSiteID),
		OWUAccountID: strings.TrimSpace(target.OWUAccountID),
		CreatedAt:    now,
	}
	if _, err = s.conn.ExecContext(ctx, `
		INSERT INTO installations(
			singleton, installation_id, owner_id, owu_site_id, owu_account_id, created_at_ns
		) VALUES (1, ?, ?, ?, ?, ?)`,
		installation.ID, installation.OwnerID, installation.OWUSiteID,
		installation.OWUAccountID, now.UnixNano()); err != nil {
		return nil, Installation{}, fmt.Errorf("persist installation identity: %w", err)
	}
	s.installation = installation
	if err = secureDatabaseFiles(abs); err != nil {
		return nil, Installation{}, err
	}
	cleanup = false
	return s, installation, nil
}

// Open opens an initialized database without requiring network credentials.
// Call VerifyTargetIdentity with a freshly authenticated OWU identity before
// any downstream access.
func Open(ctx context.Context, databasePath string) (*Store, Installation, error) {
	abs, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, Installation{}, fmt.Errorf("resolve database path: %w", err)
	}
	if err := checkExistingPrivatePath(abs); err != nil {
		return nil, Installation{}, err
	}
	s, err := openDatabase(ctx, abs, false)
	if err != nil {
		return nil, Installation{}, err
	}
	installation, err := readInstallation(ctx, s.conn)
	if err != nil {
		_ = s.Close()
		return nil, Installation{}, err
	}
	s.installation = installation
	if _, err := s.RecoverInterrupted(ctx); err != nil {
		_ = s.Close()
		return nil, Installation{}, fmt.Errorf("recover interrupted operations: %w", err)
	}
	return s, installation, nil
}

func openDatabase(ctx context.Context, path string, allowUninitialized bool) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite driver: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, classifyOpenError(err)
	}
	s := &Store{db: db, conn: conn, path: path}
	failed := true
	defer func() {
		if failed {
			_ = s.Close()
		}
	}()
	for _, statement := range []string{
		"PRAGMA busy_timeout = 100",
		"PRAGMA foreign_keys = ON",
		"PRAGMA trusted_schema = OFF",
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return nil, classifyOpenError(err)
		}
	}
	var journalMode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return nil, classifyOpenError(err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return nil, fmt.Errorf("enable WAL mode: got %q", journalMode)
	}
	var lockingMode string
	if err := conn.QueryRowContext(ctx, "PRAGMA locking_mode = EXCLUSIVE").Scan(&lockingMode); err != nil {
		return nil, classifyOpenError(err)
	}
	if !strings.EqualFold(lockingMode, "exclusive") {
		return nil, fmt.Errorf("enable exclusive locking: got %q", lockingMode)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return nil, classifyOpenError(err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, classifyOpenError(err)
	}
	var integrity string
	if err := conn.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if integrity != "ok" {
		return nil, fmt.Errorf("%w: %s", ErrCorrupt, integrity)
	}
	if err := migrate(ctx, conn); err != nil {
		return nil, err
	}
	foreignRows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, fmt.Errorf("check database references: %w", err)
	}
	if foreignRows.Next() {
		_ = foreignRows.Close()
		return nil, fmt.Errorf("%w: foreign key violations found", ErrCorrupt)
	}
	if err := foreignRows.Close(); err != nil {
		return nil, fmt.Errorf("close reference check: %w", err)
	}
	if err := foreignRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reference check: %w", err)
	}
	if !allowUninitialized {
		if _, err := readInstallation(ctx, conn); err != nil {
			return nil, err
		}
	}
	if err := secureDatabaseFiles(path); err != nil {
		return nil, err
	}
	failed = false
	return s, nil
}

func (s *Store) Installation() Installation { return s.installation }

func (s *Store) VerifyTargetIdentity(target TargetIdentity) error {
	if err := validateTargetIdentity(target); err != nil {
		return err
	}
	if strings.TrimSpace(target.OWUSiteID) != s.installation.OWUSiteID ||
		strings.TrimSpace(target.OWUAccountID) != s.installation.OWUAccountID {
		return ErrIdentityMismatch
	}
	return nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.conn != nil {
			s.closeErr = s.conn.Close()
		}
		if s.db != nil {
			if err := s.db.Close(); s.closeErr == nil {
				s.closeErr = err
			}
		}
	})
	return s.closeErr
}

func readInstallation(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (Installation, error) {
	var installation Installation
	var createdNS int64
	err := q.QueryRowContext(ctx, `
		SELECT installation_id, owner_id, owu_site_id, owu_account_id, created_at_ns
		FROM installations WHERE singleton = 1`).Scan(
		&installation.ID, &installation.OwnerID, &installation.OWUSiteID,
		&installation.OWUAccountID, &createdNS)
	if errors.Is(err, sql.ErrNoRows) {
		return Installation{}, ErrNotInitialized
	}
	if err != nil {
		return Installation{}, fmt.Errorf("read installation identity: %w", err)
	}
	installation.CreatedAt = time.Unix(0, createdNS).UTC()
	if installation.ID == "" || installation.OwnerID == "" || installation.OWUSiteID == "" || installation.OWUAccountID == "" {
		return Installation{}, fmt.Errorf("%w: installation identity is incomplete", ErrCorrupt)
	}
	return installation, nil
}

func validateTargetIdentity(target TargetIdentity) error {
	if strings.TrimSpace(target.OWUSiteID) == "" || strings.TrimSpace(target.OWUAccountID) == "" {
		return errors.New("OWU site and account identities are required")
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect data directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("data directory must be a real directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: data directory must be 0700 or stricter", ErrInsecurePermissions)
	}
	return nil
}

func checkExistingPrivatePath(path string) error {
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotInitialized
	}
	if err != nil {
		return fmt.Errorf("inspect database: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("database must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: database must be 0600 or stricter", ErrInsecurePermissions)
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		sidecarInfo, err := os.Lstat(sidecar)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect SQLite sidecar: %w", err)
		}
		if !sidecarInfo.Mode().IsRegular() || sidecarInfo.Mode()&os.ModeSymlink != 0 {
			return errors.New("SQLite sidecars must be regular files")
		}
		if sidecarInfo.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("%w: SQLite sidecars must be 0600 or stricter", ErrInsecurePermissions)
		}
	}
	return nil
}

func secureDatabaseFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect SQLite file: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("SQLite files must be regular files")
		}
		if err := os.Chmod(candidate, 0o600); err != nil {
			return fmt.Errorf("protect SQLite file: %w", err)
		}
	}
	return nil
}

func classifyOpenError(err error) error {
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "database is locked") || strings.Contains(lower, "busy") {
		return fmt.Errorf("%w: %v", ErrDatabaseInUse, err)
	}
	if strings.Contains(lower, "not a database") || strings.Contains(lower, "malformed") ||
		strings.Contains(lower, "file is encrypted") {
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return fmt.Errorf("open database: %w", err)
}

func newID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate installation identity: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}
