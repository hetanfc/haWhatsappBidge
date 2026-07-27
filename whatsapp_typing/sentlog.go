package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// sentLog remembers the messages we sent to the tracked chat so that a receipt,
// which only carries message ids, can be reported as "the video message from
// 21:14" instead of something opaque.
//
// Only the id, the timestamp and the type are stored. Message content is never
// read nor saved: the labels are built from the type alone.
type sentLog struct {
	db  *sql.DB
	log logger
}

// logger is the little slice of *slog.Logger this file needs.
type logger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}

// sentRetention is how far back the labels stay resolvable. Older rows are
// dropped: a receipt for a two-month-old message just loses its detail.
const sentRetention = 60 * 24 * time.Hour

type sentMessage struct {
	ID   string
	At   time.Time
	Kind string
}

func newSentLog(ctx context.Context, db *sql.DB, log logger) (*sentLog, error) {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS wt_sent (
			id   TEXT PRIMARY KEY,
			at   INTEGER NOT NULL,
			kind TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS wt_sent_at ON wt_sent (at)`,
		`CREATE TABLE IF NOT EXISTS wt_receipts (
			id    TEXT NOT NULL,
			type  TEXT NOT NULL,
			count INTEGER NOT NULL,
			PRIMARY KEY (id, type)
		)`,
	}
	for _, q := range schema {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("create sent log tables: %w", err)
		}
	}
	return &sentLog{db: db, log: log}, nil
}

func (s *sentLog) add(ctx context.Context, id string, at time.Time, kind string) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO wt_sent (id, at, kind) VALUES (?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		id, at.Unix(), kind)
	if err != nil {
		s.log.Warn("could not remember sent message", "err", err)
	}
}

// lookup returns the messages we know about among ids, newest first.
func (s *sentLog) lookup(ctx context.Context, ids []string) []sentMessage {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	q := `SELECT id, at, kind FROM wt_sent WHERE id IN (?` +
		strings.Repeat(",?", len(ids)-1) + `) ORDER BY at DESC`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		s.log.Warn("sent message lookup failed", "err", err)
		return nil
	}
	defer rows.Close()

	var out []sentMessage
	for rows.Next() {
		var m sentMessage
		var unix int64
		if err := rows.Scan(&m.ID, &unix, &m.Kind); err != nil {
			s.log.Warn("sent message scan failed", "err", err)
			return out
		}
		m.At = time.Unix(unix, 0)
		out = append(out, m)
	}
	return out
}

// bump counts how many times this receipt type was seen for these messages and
// returns the highest count, so a video watched three times can say so.
func (s *sentLog) bump(ctx context.Context, ids []string, receipt string) int {
	high := 0
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO wt_receipts (id, type, count) VALUES (?, ?, 1)
			 ON CONFLICT(id, type) DO UPDATE SET count = count + 1`, id, receipt); err != nil {
			s.log.Warn("could not count receipt", "err", err)
			continue
		}
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT count FROM wt_receipts WHERE id = ? AND type = ?`, id, receipt).Scan(&n); err != nil {
			continue
		}
		if n > high {
			high = n
		}
	}
	return high
}

func (s *sentLog) prune(ctx context.Context) {
	cutoff := time.Now().Add(-sentRetention).Unix()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM wt_receipts WHERE id IN (SELECT id FROM wt_sent WHERE at < ?)`, cutoff); err != nil {
		s.log.Warn("receipt prune failed", "err", err)
		return
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM wt_sent WHERE at < ?`, cutoff); err != nil {
		s.log.Warn("sent message prune failed", "err", err)
	}
}

// describeTargets turns the messages a receipt refers to into words. total is
// how many ids the receipt carried, which can be more than what we know: a
// message sent before this add-on existed has no row here.
func describeTargets(msgs []sentMessage, total int, now time.Time) string {
	switch {
	case len(msgs) == 0 && total > 1:
		return fmt.Sprintf("%d messaggi", total)
	case len(msgs) == 0:
		return ""
	case len(msgs) == 1 && total == 1:
		return msgs[0].Kind + " " + whenPhrase(msgs[0].At, now)
	default:
		return fmt.Sprintf("%d messaggi (l'ultimo: %s %s)",
			total, msgs[0].Kind, whenPhrase(msgs[0].At, now))
	}
}

// whenPhrase reads naturally inside a label: "foto delle 17:02".
func whenPhrase(at, now time.Time) string {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch {
	case !at.Before(day):
		return "delle " + at.Format("15:04")
	case !at.Before(day.AddDate(0, 0, -1)):
		return "di ieri alle " + at.Format("15:04")
	default:
		return "del " + at.Format("02/01") + " alle " + at.Format("15:04")
	}
}
