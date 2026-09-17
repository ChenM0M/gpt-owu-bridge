// Package connector exposes the single-owner bridge through authenticated MCP.
package connector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	oauth "github.com/go-oauth2/oauth2/v4"
	"github.com/go-oauth2/oauth2/v4/models"
	_ "modernc.org/sqlite"
)

type tokenStore struct {
	db *sql.DB
	tx *sql.Tx
}

func openTokens(dir string) (*tokenStore, error) {
	p := filepath.Join(dir, "oauth.sqlite")
	f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	f.Close()
	if e = os.Chmod(p, 0600); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", p)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA journal_mode=DELETE; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS spent_refresh (hash TEXT PRIMARY KEY, client TEXT NOT NULL, family TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS clients (id TEXT PRIMARY KEY, data BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS tokens (kind TEXT NOT NULL, hash TEXT NOT NULL, data BLOB NOT NULL, PRIMARY KEY(kind,hash));`)
	if e != nil {
		db.Close()
		return nil, e
	}
	return &tokenStore{db: db}, nil
}
func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (s *tokenStore) GetByID(ctx context.Context, id string) (oauth.ClientInfo, error) {
	var raw []byte
	e := s.query().QueryRowContext(ctx, "SELECT data FROM clients WHERE id=?", id).Scan(&raw)
	if e != nil {
		return nil, e
	}
	var c models.Client
	e = json.Unmarshal(raw, &c)
	return &c, e
}
func (s *tokenStore) addClient(ctx context.Context, c *models.Client) error {
	var n int
	if e := s.query().QueryRowContext(ctx, "SELECT count(*) FROM clients").Scan(&n); e != nil {
		return e
	}
	if n >= 128 {
		return errors.New("client limit reached")
	}
	b, e := json.Marshal(c)
	if e != nil {
		return e
	}
	_, e = s.query().ExecContext(ctx, "INSERT INTO clients VALUES (?,?)", c.ID, b)
	return e
}
func (s *tokenStore) Create(ctx context.Context, t oauth.TokenInfo) error {
	b, e := json.Marshal(t)
	if e != nil {
		return e
	}
	var tx *sql.Tx
	var own bool
	if s.tx != nil {
		tx = s.tx
	} else {
		tx, e = s.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		own = true
		defer tx.Rollback()
	}
	for k, v := range map[string]string{"code": t.GetCode(), "access": t.GetAccess(), "refresh": t.GetRefresh()} {
		if v != "" {
			if _, e = tx.ExecContext(ctx, "INSERT INTO tokens VALUES (?,?,?)", k, digest(v), b); e != nil {
				return e
			}
		}
	}
	if own {
		return tx.Commit()
	}
	return nil
}

func (s *tokenStore) get(ctx context.Context, k, v string) (oauth.TokenInfo, error) {
	if v == "" {
		return nil, errors.New("missing token")
	}
	var b []byte
	e := s.query().QueryRowContext(ctx, "SELECT data FROM tokens WHERE kind=? AND hash=?", k, digest(v)).Scan(&b)
	if e != nil {
		return nil, e
	}
	t := models.NewToken()
	e = json.Unmarshal(b, t)
	return t, e
}
func (s *tokenStore) remove(ctx context.Context, k, v string) error {
	_, e := s.query().ExecContext(ctx, "DELETE FROM tokens WHERE kind=? AND hash=?", k, digest(v))
	return e
}
func (s *tokenStore) GetByCode(c context.Context, v string) (oauth.TokenInfo, error) {
	return s.get(c, "code", v)
}
func (s *tokenStore) GetByAccess(c context.Context, v string) (oauth.TokenInfo, error) {
	return s.get(c, "access", v)
}
func (s *tokenStore) GetByRefresh(c context.Context, v string) (oauth.TokenInfo, error) {
	return s.get(c, "refresh", v)
}
func (s *tokenStore) RemoveByCode(c context.Context, v string) error { return s.remove(c, "code", v) }
func (s *tokenStore) RemoveByAccess(c context.Context, v string) error {
	return s.remove(c, "access", v)
}
func (s *tokenStore) RemoveByRefresh(c context.Context, v string) error {
	old, err := s.GetByRefresh(c, v)
	if err == nil {
		if ext, ok := old.(oauth.ExtendableTokenInfo); ok {
			if _, err = s.query().ExecContext(c, "INSERT OR IGNORE INTO spent_refresh VALUES (?,?,?)", digest(v), old.GetClientID(), ext.GetExtension().Get("family")); err != nil {
				return err
			}
		}
	}
	return s.remove(c, "refresh", v)
}

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *tokenStore) query() sqlExecutor {
	if s.tx != nil {
		return s.tx
	}
	return s.db
}
