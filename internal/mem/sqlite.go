package mem

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SQLite is the fallback backend: an FTS5 table in qilla.db. No conflicts
// support (Conflicts returns nothing; Judge is a no-op).
type SQLite struct {
	db  *sql.DB
	Now func() time.Time
}

// NewSQLite prepares the tables.
func NewSQLite(db *sql.DB) (*SQLite, error) {
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS mem_notes(id INTEGER PRIMARY KEY, project TEXT NOT NULL, kind TEXT NOT NULL, key TEXT NOT NULL DEFAULT '',
  text TEXT NOT NULL, support INTEGER NOT NULL DEFAULT 1, created INTEGER NOT NULL, updated INTEGER NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS mem_notes_key ON mem_notes(project, kind, key) WHERE key != '';
CREATE VIRTUAL TABLE IF NOT EXISTS mem_fts USING fts5(text, content='mem_notes', content_rowid='id');
CREATE TRIGGER IF NOT EXISTS mem_ai AFTER INSERT ON mem_notes BEGIN INSERT INTO mem_fts(rowid, text) VALUES (new.id, new.text); END;
CREATE TRIGGER IF NOT EXISTS mem_ad AFTER DELETE ON mem_notes BEGIN INSERT INTO mem_fts(mem_fts, rowid, text) VALUES('delete', old.id, old.text); END;
CREATE TRIGGER IF NOT EXISTS mem_au AFTER UPDATE ON mem_notes BEGIN
  INSERT INTO mem_fts(mem_fts, rowid, text) VALUES('delete', old.id, old.text);
  INSERT INTO mem_fts(rowid, text) VALUES (new.id, new.text); END;`); err != nil {
		return nil, err
	}
	return &SQLite{db: db, Now: time.Now}, nil
}

func (s *SQLite) Name() string { return "sqlite" }

func (s *SQLite) Save(ctx context.Context, project, kind, key, text string) (Entry, error) {
	now := s.Now().Unix()
	if key != "" {
		var id int64
		err := s.db.QueryRowContext(ctx, `SELECT id FROM mem_notes WHERE project=? AND kind=? AND key=?`, project, kind, key).Scan(&id)
		if err == nil {
			if _, err := s.db.ExecContext(ctx, `UPDATE mem_notes SET text=?, support=support+1, updated=? WHERE id=?`, text, now, id); err != nil {
				return Entry{}, err
			}
			return s.Get(ctx, strconv.FormatInt(id, 10))
		}
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO mem_notes(project,kind,key,text,created,updated) VALUES(?,?,?,?,?,?)`, project, kind, key, text, now, now)
	if err != nil {
		return Entry{}, err
	}
	id, _ := res.LastInsertId()
	return s.Get(ctx, strconv.FormatInt(id, 10))
}

const cols = `mem_notes.id,mem_notes.project,mem_notes.kind,mem_notes.key,mem_notes.text,mem_notes.support,mem_notes.created,mem_notes.updated`

type scanner interface{ Scan(...any) error }

// scan decodes the cols columns.
func scan(r scanner) (Entry, error) {
	e, _, err := decode(r, false)
	return e, err
}

// scanRanked decodes cols plus a trailing bm25 rank.
func scanRanked(r scanner) (Entry, float64, error) { return decode(r, true) }

func decode(r scanner, ranked bool) (Entry, float64, error) {
	var e Entry
	var id, c, u int64
	var rank float64
	dst := []any{&id, &e.Project, &e.Kind, &e.Key, &e.Text, &e.Support, &c, &u}
	if ranked {
		dst = append(dst, &rank)
	}
	if err := r.Scan(dst...); err != nil {
		return Entry{}, 0, err
	}
	e.ID = strconv.FormatInt(id, 10)
	e.Created, e.Updated = time.Unix(c, 0), time.Unix(u, 0)
	return e, rank, nil
}

func (s *SQLite) Search(ctx context.Context, project, kind, query string, n int) ([]Entry, error) {
	q := ftsQuery(query)
	if q == "" {
		return nil, nil
	}
	sqlq := `SELECT ` + cols + `, bm25(mem_fts) FROM mem_fts JOIN mem_notes ON mem_notes.id = mem_fts.rowid WHERE mem_fts MATCH ?`
	args := []any{q}
	if project != "" {
		sqlq += ` AND mem_notes.project=?`
		args = append(args, project)
	}
	if kind != "" {
		sqlq += ` AND mem_notes.kind=?`
		args = append(args, kind)
	}
	sqlq += ` ORDER BY bm25(mem_fts) LIMIT ?`
	args = append(args, n)
	rows, err := s.db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, rank, err := scanRanked(rows)
		if err != nil {
			return nil, err
		}
		e.Score = -rank
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLite) Get(ctx context.Context, id string) (Entry, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM mem_notes WHERE id=?`, id))
}

func (s *SQLite) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mem_notes WHERE id=?`, id)
	return err
}

func (s *SQLite) Recent(ctx context.Context, project string, n int) ([]Entry, error) {
	q := `SELECT ` + cols + ` FROM mem_notes`
	var args []any
	if project != "" {
		q += ` WHERE project=?`
		args = append(args, project)
	}
	q += ` ORDER BY updated DESC LIMIT ?`
	args = append(args, n)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLite) Conflicts(context.Context, string) ([]Conflict, error) { return nil, nil }
func (s *SQLite) Judge(context.Context, string, string, string) error   { return nil }
func (s *SQLite) Health(context.Context) error                          { return nil }

// ftsQuery turns free text into an OR-joined FTS5 query of quoted terms.
func ftsQuery(q string) string {
	var terms []string
	for _, w := range strings.Fields(q) {
		w = strings.Trim(w, `"'.,;:!?()[]{}`)
		if w == "" {
			continue
		}
		terms = append(terms, fmt.Sprintf("%q", w))
	}
	return strings.Join(terms, " OR ")
}
