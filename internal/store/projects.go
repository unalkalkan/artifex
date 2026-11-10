package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Project roles, from least to most.
const (
	ProjectCommenter = "commenter"
	ProjectEditor    = "editor"
	ProjectOwner     = "owner"
)

// ErrLastOwner is returned when a change would leave a project with no Owner.
var ErrLastOwner = errors.New("a project must keep at least one owner")

type Project struct {
	ID        string
	CreatedAt time.Time
	CreatedBy int64
}

// ProjectMembership is a project with the account's role in it.
type ProjectMembership struct {
	Project
	Role string // "" when the account is not a member (Admins listing all)
}

type Member struct {
	ProjectID string
	Account   Account
	Role      string
	AddedAt   time.Time
}

type Invite struct {
	ProjectID string
	Email     string
	Role      string
	CreatedAt time.Time
}

// CreateProject records a project and makes ownerID its Owner, in one transaction.
func (s *Store) CreateProject(ctx context.Context, id string, ownerID int64) error {
	now := s.unix()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects (id, created_at, created_by) VALUES (?, ?, ?)`, id, now, ownerID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO project_members (project_id, account_id, role, added_at) VALUES (?, ?, 'owner', ?)`, id, ownerID, now)
		return err
	})
}

func (s *Store) DeleteProject(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	return err
}

func (s *Store) ProjectByID(ctx context.Context, id string) (Project, error) {
	var p Project
	var created int64
	var by sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, created_at, created_by FROM projects WHERE id = ?`, id).Scan(&p.ID, &created, &by)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	p.CreatedAt = time.Unix(created, 0).UTC()
	p.CreatedBy = by.Int64
	return p, err
}

// ListProjects lists the projects accountID belongs to; all projects, with
// the account's role where it has one, when all is true.
func (s *Store) ListProjects(ctx context.Context, accountID int64, all bool) ([]ProjectMembership, error) {
	q := `SELECT p.id, p.created_at, p.created_by, COALESCE(m.role, '') FROM projects p
	      LEFT JOIN project_members m ON m.project_id = p.id AND m.account_id = ?`
	if !all {
		q += ` WHERE m.role IS NOT NULL`
	}
	q += ` ORDER BY p.id`
	res, err := s.db.QueryContext(ctx, q, accountID)
	if err != nil {
		return nil, err
	}
	defer res.Close()
	var out []ProjectMembership
	for res.Next() {
		var pm ProjectMembership
		var created int64
		var by sql.NullInt64
		if err := res.Scan(&pm.ID, &created, &by, &pm.Role); err != nil {
			return nil, err
		}
		pm.CreatedAt = time.Unix(created, 0).UTC()
		pm.CreatedBy = by.Int64
		out = append(out, pm)
	}
	return out, res.Err()
}

// MemberRole returns the account's role in the project, "" when it has none.
func (s *Store) MemberRole(ctx context.Context, projectID string, accountID int64) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM project_members WHERE project_id = ? AND account_id = ?`, projectID, accountID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

func (s *Store) ListMembers(ctx context.Context, projectID string) ([]Member, error) {
	res, err := s.db.QueryContext(ctx, `SELECT m.project_id, m.role, m.added_at, `+prefixed("a", accountCols)+`
		FROM project_members m JOIN accounts a ON a.id = m.account_id WHERE m.project_id = ?
		ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'editor' THEN 1 ELSE 2 END, a.email`, projectID)
	if err != nil {
		return nil, err
	}
	defer res.Close()
	var out []Member
	for res.Next() {
		var m Member
		var added, created int64
		var verified, seen sql.NullInt64
		a := &m.Account
		if err := res.Scan(&m.ProjectID, &m.Role, &added, &a.ID, &a.Email, &a.Name, &a.Role, &a.Status, &created, &verified, &seen); err != nil {
			return nil, err
		}
		m.AddedAt = time.Unix(added, 0).UTC()
		a.CreatedAt = time.Unix(created, 0).UTC()
		a.VerifiedAt = fromUnix(verified)
		a.LastSeenAt = fromUnix(seen)
		out = append(out, m)
	}
	return out, res.Err()
}

// SetMember adds the account to the project or changes its role. Demoting
// the last Owner fails with ErrLastOwner.
func (s *Store) SetMember(ctx context.Context, projectID string, accountID int64, role string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var cur string
		err := tx.QueryRowContext(ctx, `SELECT role FROM project_members WHERE project_id = ? AND account_id = ?`, projectID, accountID).Scan(&cur)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if cur == ProjectOwner && role != ProjectOwner {
			if err := lastOwnerCheck(ctx, tx, projectID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO project_members (project_id, account_id, role, added_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (project_id, account_id) DO UPDATE SET role = excluded.role`, projectID, accountID, role, s.unix())
		return err
	})
}

// RemoveMember removes the account from the project; removing the last
// Owner fails with ErrLastOwner.
func (s *Store) RemoveMember(ctx context.Context, projectID string, accountID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var cur string
		err := tx.QueryRowContext(ctx, `SELECT role FROM project_members WHERE project_id = ? AND account_id = ?`, projectID, accountID).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur == ProjectOwner {
			if err := lastOwnerCheck(ctx, tx, projectID); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM project_members WHERE project_id = ? AND account_id = ?`, projectID, accountID)
		return err
	})
}

func lastOwnerCheck(ctx context.Context, tx *sql.Tx, projectID string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_members WHERE project_id = ? AND role = 'owner'`, projectID).Scan(&n); err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastOwner
	}
	return nil
}

// SoleOwnerOf lists the projects where the account is the only Owner.
func (s *Store) SoleOwnerOf(ctx context.Context, accountID int64) ([]string, error) {
	res, err := s.db.QueryContext(ctx, `SELECT m.project_id FROM project_members m WHERE m.account_id = ? AND m.role = 'owner'
		AND (SELECT COUNT(*) FROM project_members o WHERE o.project_id = m.project_id AND o.role = 'owner') = 1 ORDER BY m.project_id`, accountID)
	if err != nil {
		return nil, err
	}
	defer res.Close()
	var out []string
	for res.Next() {
		var id string
		if err := res.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, res.Err()
}

func (s *Store) SetInvite(ctx context.Context, projectID, email, role string, by int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO project_invites (project_id, email, role, invited_by, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (project_id, email) DO UPDATE SET role = excluded.role`, projectID, strings.ToLower(strings.TrimSpace(email)), role, by, s.unix())
	return err
}

func (s *Store) DeleteInvite(ctx context.Context, projectID, email string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM project_invites WHERE project_id = ? AND email = ?`, projectID, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListInvites(ctx context.Context, projectID string) ([]Invite, error) {
	res, err := s.db.QueryContext(ctx, `SELECT project_id, email, role, created_at FROM project_invites WHERE project_id = ? ORDER BY email`, projectID)
	if err != nil {
		return nil, err
	}
	defer res.Close()
	var out []Invite
	for res.Next() {
		var in Invite
		var created int64
		if err := res.Scan(&in.ProjectID, &in.Email, &in.Role, &created); err != nil {
			return nil, err
		}
		in.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, in)
	}
	return out, res.Err()
}

// ApplyInvites turns the account's waiting invitations into memberships. An
// invitation never lowers a role the account already has.
func (s *Store) ApplyInvites(ctx context.Context, a Account) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.QueryContext(ctx, `SELECT project_id, role FROM project_invites WHERE email = ?`, a.Email)
		if err != nil {
			return err
		}
		type inv struct{ project, role string }
		var invs []inv
		for res.Next() {
			var i inv
			if err := res.Scan(&i.project, &i.role); err != nil {
				res.Close()
				return err
			}
			invs = append(invs, i)
		}
		res.Close()
		for _, i := range invs {
			var cur string
			err := tx.QueryRowContext(ctx, `SELECT role FROM project_members WHERE project_id = ? AND account_id = ?`, i.project, a.ID).Scan(&cur)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if RoleRank(cur) < RoleRank(i.role) {
				if _, err := tx.ExecContext(ctx, `INSERT INTO project_members (project_id, account_id, role, added_at) VALUES (?, ?, ?, ?)
					ON CONFLICT (project_id, account_id) DO UPDATE SET role = excluded.role`, i.project, a.ID, i.role, s.unix()); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM project_invites WHERE project_id = ? AND email = ?`, i.project, a.Email); err != nil {
				return err
			}
		}
		return nil
	})
}

// RoleRank orders project roles: none < commenter < editor < owner.
func RoleRank(role string) int {
	switch role {
	case ProjectCommenter:
		return 1
	case ProjectEditor:
		return 2
	case ProjectOwner:
		return 3
	}
	return 0
}

// ValidProjectRole reports whether role is a project role.
func ValidProjectRole(role string) bool { return RoleRank(role) > 0 }
