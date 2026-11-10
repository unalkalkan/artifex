package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Account roles and statuses.
const (
	RoleMember = "member"
	RoleAdmin  = "admin"

	StatusPending  = "pending"
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

type Account struct {
	ID         int64
	Email      string
	Name       string
	Role       string
	Status     string
	CreatedAt  time.Time
	VerifiedAt *time.Time
	LastSeenAt *time.Time
}

func (a Account) IsAdmin() bool  { return a.Role == RoleAdmin }
func (a Account) IsActive() bool { return a.Status == StatusActive }

// DisplayName is the name, or the email when there is none.
func (a Account) DisplayName() string {
	if strings.TrimSpace(a.Name) != "" {
		return a.Name
	}
	return a.Email
}

const accountCols = `id, email, name, role, status, created_at, verified_at, last_seen_at`

type scanner interface{ Scan(...any) error }

func scanAccount(sc scanner) (Account, error) {
	var a Account
	var created int64
	var verified, seen sql.NullInt64
	if err := sc.Scan(&a.ID, &a.Email, &a.Name, &a.Role, &a.Status, &created, &verified, &seen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return a, ErrNotFound
		}
		return a, err
	}
	a.CreatedAt = time.Unix(created, 0).UTC()
	a.VerifiedAt = fromUnix(verified)
	a.LastSeenAt = fromUnix(seen)
	return a, nil
}

// CreateAccount adds an account. Active accounts count as verified.
func (s *Store) CreateAccount(ctx context.Context, email, name, role, status string) (Account, error) {
	now := s.unix()
	var verified any
	if status == StatusActive {
		verified = now
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO accounts (email, name, role, status, created_at, verified_at) VALUES (?, ?, ?, ?, ?, ?)`,
		strings.ToLower(strings.TrimSpace(email)), strings.TrimSpace(name), role, status, now, verified)
	if err != nil {
		return Account{}, err
	}
	id, _ := res.LastInsertId()
	return s.AccountByID(ctx, id)
}

func (s *Store) AccountByID(ctx context.Context, id int64) (Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = ?`, id))
}

func (s *Store) AccountByEmail(ctx context.Context, email string) (Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE email = ?`, strings.ToLower(strings.TrimSpace(email))))
}

func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	res, err := s.db.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts ORDER BY status, email`)
	if err != nil {
		return nil, err
	}
	defer res.Close()
	var out []Account
	for res.Next() {
		a, err := scanAccount(res)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, res.Err()
}

// SetAccountStatus changes the status; activating a pending account marks it verified.
func (s *Store) SetAccountStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET status = ?, verified_at = COALESCE(verified_at, CASE WHEN ? = 'active' THEN ? END) WHERE id = ?`,
		status, status, s.unix(), id)
	return err
}

func (s *Store) SetAccountRole(ctx context.Context, id int64, role string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET role = ? WHERE id = ?`, role, id)
	return err
}

func (s *Store) SetAccountName(ctx context.Context, id int64, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET name = ? WHERE id = ?`, strings.TrimSpace(name), id)
	return err
}

func (s *Store) TouchAccount(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET last_seen_at = ? WHERE id = ?`, s.unix(), id)
	return err
}

// DeleteAccount removes an account, its sessions, tokens and memberships.
func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	return err
}

// Session is a signed-in browser.
type Session struct {
	AccountID int64
	CSRF      string
	ExpiresAt time.Time
}

func (s *Store) CreateSession(ctx context.Context, idHash string, accountID int64, csrf string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id_hash, account_id, csrf, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		idHash, accountID, csrf, s.unix(), expires.Unix())
	return err
}

// SessionByHash returns an unexpired session and its account.
func (s *Store) SessionByHash(ctx context.Context, idHash string) (Session, Account, error) {
	var sess Session
	var exp int64
	q := `SELECT s.account_id, s.csrf, s.expires_at, ` + prefixed("a", accountCols) + ` FROM sessions s JOIN accounts a ON a.id = s.account_id WHERE s.id_hash = ? AND s.expires_at > ?`
	var a Account
	var created int64
	var verified, seen sql.NullInt64
	err := s.db.QueryRowContext(ctx, q, idHash, s.unix()).Scan(&sess.AccountID, &sess.CSRF, &exp,
		&a.ID, &a.Email, &a.Name, &a.Role, &a.Status, &created, &verified, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, a, ErrNotFound
	}
	if err != nil {
		return sess, a, err
	}
	sess.ExpiresAt = time.Unix(exp, 0)
	a.CreatedAt = time.Unix(created, 0).UTC()
	a.VerifiedAt = fromUnix(verified)
	a.LastSeenAt = fromUnix(seen)
	return sess, a, nil
}

func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash)
	return err
}

func (s *Store) DeleteSessionsFor(ctx context.Context, accountID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE account_id = ?`, accountID)
	return err
}

func (s *Store) PurgeExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, s.unix())
	return err
}

// Token is an agent's credential. Only its hash is kept.
type Token struct {
	ID         int64
	AccountID  int64
	Label      string
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

func (s *Store) CreateToken(ctx context.Context, accountID int64, label, hash, prefix string) (Token, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO tokens (account_id, label, hash, prefix, created_at) VALUES (?, ?, ?, ?, ?)`,
		accountID, label, hash, prefix, s.unix())
	if err != nil {
		return Token{}, err
	}
	id, _ := res.LastInsertId()
	return s.TokenByID(ctx, id)
}

const tokenCols = `id, account_id, label, prefix, created_at, last_used_at, revoked_at`

func scanToken(sc scanner) (Token, error) {
	var t Token
	var created int64
	var used, revoked sql.NullInt64
	if err := sc.Scan(&t.ID, &t.AccountID, &t.Label, &t.Prefix, &created, &used, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return t, ErrNotFound
		}
		return t, err
	}
	t.CreatedAt = time.Unix(created, 0).UTC()
	t.LastUsedAt = fromUnix(used)
	t.RevokedAt = fromUnix(revoked)
	return t, nil
}

func (s *Store) TokenByID(ctx context.Context, id int64) (Token, error) {
	return scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE id = ?`, id))
}

// TokenByHash returns a token that is not revoked, and its account.
func (s *Store) TokenByHash(ctx context.Context, hash string) (Token, Account, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE hash = ? AND revoked_at IS NULL`, hash))
	if err != nil {
		return t, Account{}, err
	}
	a, err := s.AccountByID(ctx, t.AccountID)
	return t, a, err
}

// ListTokens lists one account's tokens, or everyone's when accountID is 0.
func (s *Store) ListTokens(ctx context.Context, accountID int64) ([]Token, error) {
	q := `SELECT ` + tokenCols + ` FROM tokens`
	var args []any
	if accountID != 0 {
		q += ` WHERE account_id = ?`
		args = append(args, accountID)
	}
	q += ` ORDER BY revoked_at IS NOT NULL, created_at DESC`
	res, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer res.Close()
	var out []Token
	for res.Next() {
		t, err := scanToken(res)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, res.Err()
}

func (s *Store) RevokeToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, s.unix(), id)
	return err
}

func (s *Store) TouchToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, s.unix(), id)
	return err
}

func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
