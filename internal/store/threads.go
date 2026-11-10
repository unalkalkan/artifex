package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Thread states and entry kinds.
const (
	ThreadOpen     = "open"
	ThreadResolved = "resolved"

	EntryComment  = "comment"
	EntryResolved = "resolved"
	EntryReopened = "reopened"
)

// Thread is a comment and its replies, pinned to a spot on an artifact at
// the commit it was made on (design decision 14).
type Thread struct {
	ID             int64
	ProjectID      string
	Path           string // the path the thread was made on
	CommitID       string // the version it was made on
	Selector       string
	ElementText    string
	X, Y           *float64 // point in the artifact's own pixels
	OX, OY         *float64 // the point's offset inside the element
	ViewWidth      *int     // the width the artifact was shown at
	State          string
	ResolvedCommit string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Entries        []ThreadEntry
}

// ThreadEntry is one comment, or a resolve or reopen, in a thread.
type ThreadEntry struct {
	ID         int64
	ThreadID   int64
	Kind       string
	AccountID  int64 // 0 when the account was removed
	AuthorName string
	AgentLabel string // the token's label when an agent wrote it
	Body       string
	CommitID   string // for resolves: the version with the change
	CreatedAt  time.Time
}

// Author is "<label> for <name>" for agents, and the name for people.
func (e ThreadEntry) Author() string {
	if e.AgentLabel != "" {
		return e.AgentLabel + " for " + e.AuthorName
	}
	return e.AuthorName
}

// CreateThread stores a thread with its first comment.
func (s *Store) CreateThread(ctx context.Context, t Thread, first ThreadEntry) (int64, error) {
	now := s.unix()
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO threads (project_id, path, commit_id, selector, element_text, x, y, ox, oy, view_width, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?)`,
			t.ProjectID, t.Path, t.CommitID, t.Selector, t.ElementText, t.X, t.Y, t.OX, t.OY, t.ViewWidth, now, now)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		first.ThreadID = id
		first.Kind = EntryComment
		return insertEntry(ctx, tx, first, now)
	})
	return id, err
}

func insertEntry(ctx context.Context, tx *sql.Tx, e ThreadEntry, now int64) error {
	var acct any
	if e.AccountID != 0 {
		acct = e.AccountID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO thread_entries (thread_id, kind, account_id, author_name, agent_label, body, commit_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, e.ThreadID, e.Kind, acct, e.AuthorName, e.AgentLabel, e.Body, e.CommitID, now)
	return err
}

// AddEntry appends a comment, resolve or reopen, and updates the thread's state.
func (s *Store) AddEntry(ctx context.Context, e ThreadEntry) error {
	now := s.unix()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := insertEntry(ctx, tx, e, now); err != nil {
			return err
		}
		switch e.Kind {
		case EntryResolved:
			_, err := tx.ExecContext(ctx, `UPDATE threads SET state = 'resolved', resolved_commit = ?, updated_at = ? WHERE id = ?`, e.CommitID, now, e.ThreadID)
			return err
		case EntryReopened:
			_, err := tx.ExecContext(ctx, `UPDATE threads SET state = 'open', resolved_commit = '', updated_at = ? WHERE id = ?`, now, e.ThreadID)
			return err
		default:
			_, err := tx.ExecContext(ctx, `UPDATE threads SET updated_at = ? WHERE id = ?`, now, e.ThreadID)
			return err
		}
	})
}

const threadCols = `id, project_id, path, commit_id, selector, element_text, x, y, ox, oy, view_width, state, resolved_commit, created_at, updated_at`

func scanThread(sc scanner) (Thread, error) {
	var t Thread
	var x, y, ox, oy sql.NullFloat64
	var w sql.NullInt64
	var created, updated int64
	if err := sc.Scan(&t.ID, &t.ProjectID, &t.Path, &t.CommitID, &t.Selector, &t.ElementText, &x, &y, &ox, &oy, &w, &t.State, &t.ResolvedCommit, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return t, ErrNotFound
		}
		return t, err
	}
	f := func(v sql.NullFloat64) *float64 {
		if !v.Valid {
			return nil
		}
		x := v.Float64
		return &x
	}
	t.X, t.Y, t.OX, t.OY = f(x), f(y), f(ox), f(oy)
	if w.Valid {
		v := int(w.Int64)
		t.ViewWidth = &v
	}
	t.CreatedAt = time.Unix(created, 0).UTC()
	t.UpdatedAt = time.Unix(updated, 0).UTC()
	return t, nil
}

// ThreadByID returns a thread with its entries.
func (s *Store) ThreadByID(ctx context.Context, id int64) (Thread, error) {
	t, err := scanThread(s.db.QueryRowContext(ctx, `SELECT `+threadCols+` FROM threads WHERE id = ?`, id))
	if err != nil {
		return t, err
	}
	ts := []Thread{t}
	if err := s.loadEntries(ctx, ts); err != nil {
		return t, err
	}
	return ts[0], nil
}

// ThreadFilter selects threads. Projects is required: callers pass the
// projects the reader may see.
type ThreadFilter struct {
	Projects []string
	State    string // "" for both
	Since    time.Time
	Limit    int
}

// ListThreads returns matching threads with their entries, most recently
// changed first.
func (s *Store) ListThreads(ctx context.Context, f ThreadFilter) ([]Thread, error) {
	if len(f.Projects) == 0 {
		return nil, nil
	}
	q := `SELECT ` + threadCols + ` FROM threads WHERE project_id IN (?` + strings.Repeat(",?", len(f.Projects)-1) + `)`
	args := make([]any, 0, len(f.Projects)+3)
	for _, p := range f.Projects {
		args = append(args, p)
	}
	if f.State != "" {
		q += ` AND state = ?`
		args = append(args, f.State)
	}
	if !f.Since.IsZero() {
		q += ` AND updated_at >= ?`
		args = append(args, f.Since.Unix())
	}
	q += ` ORDER BY updated_at DESC, id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	res, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var out []Thread
	for res.Next() {
		t, err := scanThread(res)
		if err != nil {
			res.Close()
			return nil, err
		}
		out = append(out, t)
	}
	res.Close()
	if err := res.Err(); err != nil {
		return nil, err
	}
	return out, s.loadEntries(ctx, out)
}

func (s *Store) loadEntries(ctx context.Context, ts []Thread) error {
	if len(ts) == 0 {
		return nil
	}
	idx := make(map[int64]int, len(ts))
	args := make([]any, len(ts))
	for i, t := range ts {
		idx[t.ID] = i
		args[i] = t.ID
	}
	res, err := s.db.QueryContext(ctx, `SELECT id, thread_id, kind, COALESCE(account_id, 0), author_name, agent_label, body, commit_id, created_at
		FROM thread_entries WHERE thread_id IN (?`+strings.Repeat(",?", len(ts)-1)+`) ORDER BY id`, args...)
	if err != nil {
		return err
	}
	defer res.Close()
	for res.Next() {
		var e ThreadEntry
		var created int64
		if err := res.Scan(&e.ID, &e.ThreadID, &e.Kind, &e.AccountID, &e.AuthorName, &e.AgentLabel, &e.Body, &e.CommitID, &created); err != nil {
			return err
		}
		e.CreatedAt = time.Unix(created, 0).UTC()
		i := idx[e.ThreadID]
		ts[i].Entries = append(ts[i].Entries, e)
	}
	return res.Err()
}
