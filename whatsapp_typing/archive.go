package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// messageArchive is the local ledger for the single configured chat. WhatsApp
// sends reactions, edits and revocations with only the ID of the target
// message, so keeping the original message is the only way to turn those IDs
// into useful Home Assistant events.
type messageArchive struct {
	db        *sql.DB
	log       logger
	retention time.Duration
}

type archivedMessage struct {
	ID          string
	At          time.Time
	Kind        string
	Text        string
	FromMe      bool
	AgentSender string
	QuotedID    string
	Ephemeral   bool
	ViewOnce    bool
	Forwarded   bool
	Duration    int
	EditedAt    time.Time
	DeletedAt   time.Time
}

func newMessageArchive(ctx context.Context, db *sql.DB, log logger, retention time.Duration) (*messageArchive, error) {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS wt_messages (
			id         TEXT PRIMARY KEY,
			at         INTEGER NOT NULL,
			kind       TEXT NOT NULL,
			text       TEXT NOT NULL DEFAULT '',
			from_me    INTEGER NOT NULL DEFAULT 0,
			agent_sender TEXT NOT NULL DEFAULT '',
			quoted_id  TEXT NOT NULL DEFAULT '',
			ephemeral  INTEGER NOT NULL DEFAULT 0,
			view_once  INTEGER NOT NULL DEFAULT 0,
			forwarded  INTEGER NOT NULL DEFAULT 0,
			duration   INTEGER NOT NULL DEFAULT 0,
			edited_at  INTEGER NOT NULL DEFAULT 0,
			deleted_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS wt_messages_at ON wt_messages (at)`,
		`CREATE TABLE IF NOT EXISTS wt_message_revisions (
			revision_id INTEGER PRIMARY KEY AUTOINCREMENT,
			id          TEXT NOT NULL,
			at          INTEGER NOT NULL,
			kind        TEXT NOT NULL,
			text        TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS wt_message_revisions_id ON wt_message_revisions (id)`,
		`CREATE TABLE IF NOT EXISTS wt_reactions (
			target_id TEXT PRIMARY KEY,
			emoji     TEXT NOT NULL,
			at        INTEGER NOT NULL
		)`,
	}
	for _, q := range schema {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("create message archive tables: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE wt_messages ADD COLUMN agent_sender TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return nil, fmt.Errorf("add agent sender to message archive: %w", err)
	}
	// Preserve the message labels collected by versions before 1.4.0. The old
	// table only knew our outgoing ID/time/type, but that is still enough to
	// identify a reaction to one of those messages after the upgrade.
	if _, err := db.ExecContext(ctx, `INSERT INTO wt_messages (id, at, kind, from_me)
		SELECT id, at, kind, 1 FROM wt_sent WHERE true ON CONFLICT(id) DO NOTHING`); err != nil {
		log.Debug("previous sent-message log not migrated", "err", err)
	}
	return &messageArchive{db: db, log: log, retention: retention}, nil
}

func (a *messageArchive) add(ctx context.Context, m archivedMessage) {
	if m.ID == "" {
		return
	}
	_, err := a.db.ExecContext(ctx, `INSERT INTO wt_messages
		(id, at, kind, text, from_me, agent_sender, quoted_id, ephemeral, view_once, forwarded,
		 duration, edited_at, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		m.ID, m.At.Unix(), m.Kind, m.Text, boolInt(m.FromMe), m.AgentSender, m.QuotedID,
		boolInt(m.Ephemeral), boolInt(m.ViewOnce), boolInt(m.Forwarded), m.Duration,
		timeMillis(m.EditedAt), timeMillis(m.DeletedAt))
	if err != nil {
		a.log.Warn("could not archive message", "id", m.ID, "err", err)
	}
}

func (a *messageArchive) get(ctx context.Context, id string) (archivedMessage, bool) {
	if id == "" {
		return archivedMessage{}, false
	}
	var m archivedMessage
	var unix, edited, deleted int64
	var fromMe, ephemeral, viewOnce, forwarded int
	err := a.db.QueryRowContext(ctx, `SELECT id, at, kind, text, from_me, agent_sender, quoted_id,
		ephemeral, view_once, forwarded, duration, edited_at, deleted_at
		FROM wt_messages WHERE id = ?`, id).
		Scan(&m.ID, &unix, &m.Kind, &m.Text, &fromMe, &m.AgentSender, &m.QuotedID,
			&ephemeral, &viewOnce, &forwarded, &m.Duration, &edited, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return archivedMessage{}, false
	}
	if err != nil {
		a.log.Warn("message archive lookup failed", "id", id, "err", err)
		return archivedMessage{}, false
	}
	m.At = time.Unix(unix, 0)
	m.FromMe = fromMe != 0
	m.Ephemeral = ephemeral != 0
	m.ViewOnce = viewOnce != 0
	m.Forwarded = forwarded != 0
	if edited > 0 {
		m.EditedAt = time.UnixMilli(edited)
	}
	if deleted > 0 {
		m.DeletedAt = time.UnixMilli(deleted)
	}
	return m, true
}

func (a *messageArchive) markAgentMessage(
	ctx context.Context,
	id string,
	at time.Time,
	kind string,
	text string,
	agent string,
) {
	if id == "" || agent == "" {
		return
	}
	_, err := a.db.ExecContext(ctx, `INSERT INTO wt_messages
		(id, at, kind, text, from_me, agent_sender)
		VALUES (?, ?, ?, ?, 1, ?)
		ON CONFLICT(id) DO UPDATE SET
			agent_sender = excluded.agent_sender,
			text = CASE WHEN excluded.text <> '' THEN excluded.text ELSE wt_messages.text END`,
		id, at.Unix(), kind, text, agent)
	if err != nil {
		a.log.Warn("could not mark agent message", "id", id, "agent", agent, "err", err)
	}
}

func (a *messageArchive) recentBefore(
	ctx context.Context,
	currentID string,
	at time.Time,
	window time.Duration,
	limit int,
	maxChars int,
) []archivedMessage {
	if limit < 1 || maxChars < 1 {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id, at, kind, text, from_me, agent_sender,
		quoted_id, ephemeral, view_once, forwarded, duration, edited_at, deleted_at
		FROM wt_messages
		WHERE id <> ? AND at <= ? AND at >= ? AND deleted_at = 0
		  AND trim(text) <> '' AND agent_sender <> 'ack'
		ORDER BY at DESC, rowid DESC
		LIMIT ?`,
		currentID, at.Unix(), at.Add(-window).Unix(), limit)
	if err != nil {
		a.log.Warn("recent message lookup failed", "err", err)
		return nil
	}
	defer rows.Close()

	messages := make([]archivedMessage, 0, limit)
	remaining := maxChars
	for rows.Next() {
		var m archivedMessage
		var unix, edited, deleted int64
		var fromMe, ephemeral, viewOnce, forwarded int
		if err := rows.Scan(
			&m.ID, &unix, &m.Kind, &m.Text, &fromMe, &m.AgentSender,
			&m.QuotedID, &ephemeral, &viewOnce, &forwarded, &m.Duration,
			&edited, &deleted,
		); err != nil {
			a.log.Warn("recent message scan failed", "err", err)
			break
		}
		runes := []rune(strings.Join(strings.Fields(m.Text), " "))
		if len(runes) > remaining {
			runes = runes[:remaining]
		}
		m.Text = string(runes)
		if m.Text == "" {
			break
		}
		m.At = time.Unix(unix, 0)
		m.FromMe = fromMe != 0
		m.Ephemeral = ephemeral != 0
		m.ViewOnce = viewOnce != 0
		m.Forwarded = forwarded != 0
		messages = append(messages, m)
		remaining -= len(runes)
		if remaining <= 0 {
			break
		}
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	return messages
}

// edit stores the previous revision before replacing it. The returned message
// is the version that existed before the edit.
func (a *messageArchive) edit(ctx context.Context, id string, next archivedMessage, at time.Time) (archivedMessage, bool, bool) {
	old, found := a.get(ctx, id)
	if !found {
		next.ID = id
		next.At = at
		next.EditedAt = at
		a.add(ctx, next)
		return archivedMessage{}, false, true
	}
	if !old.DeletedAt.IsZero() || (!old.EditedAt.IsZero() && !at.After(old.EditedAt)) {
		return old, true, false
	}
	if old.Kind == next.Kind && old.Text == next.Text {
		return old, true, false
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		a.log.Warn("could not start message edit transaction", "err", err)
		return old, true, false
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO wt_message_revisions (id, at, kind, text)
		VALUES (?, ?, ?, ?)`, id, at.UnixMilli(), old.Kind, old.Text); err != nil {
		a.log.Warn("could not archive previous message revision", "id", id, "err", err)
		return old, true, false
	}
	if _, err = tx.ExecContext(ctx, `UPDATE wt_messages
		SET kind = ?, text = ?, quoted_id = ?, ephemeral = ?, view_once = ?,
		    forwarded = ?, duration = ?, edited_at = ?
		WHERE id = ?`,
		next.Kind, next.Text, next.QuotedID, boolInt(next.Ephemeral),
		boolInt(next.ViewOnce), boolInt(next.Forwarded), next.Duration, at.UnixMilli(), id); err != nil {
		a.log.Warn("could not update edited message", "id", id, "err", err)
		return old, true, false
	}
	if err = tx.Commit(); err != nil {
		a.log.Warn("could not commit message edit", "id", id, "err", err)
		return old, true, false
	}
	return old, true, true
}

func (a *messageArchive) delete(ctx context.Context, id string, at time.Time) (archivedMessage, bool, bool) {
	old, found := a.get(ctx, id)
	if !found {
		placeholder := archivedMessage{
			ID: id, At: at, Kind: "messaggio", DeletedAt: at,
		}
		a.add(ctx, placeholder)
		if stored, ok := a.get(ctx, id); ok && !stored.DeletedAt.IsZero() {
			return archivedMessage{}, false, true
		}
		return archivedMessage{}, false, false
	}
	if !old.DeletedAt.IsZero() {
		return old, true, false
	}
	res, err := a.db.ExecContext(ctx,
		`UPDATE wt_messages SET deleted_at = ? WHERE id = ? AND deleted_at = 0`,
		at.UnixMilli(), id)
	if err != nil {
		a.log.Warn("could not mark message deleted", "id", id, "err", err)
		return old, true, false
	}
	n, _ := res.RowsAffected()
	return old, true, n > 0
}

// reaction updates the contact's current reaction to a target. An empty emoji
// means the reaction was removed. It returns the previous emoji and whether the
// state actually changed, which filters reconnect replays.
func (a *messageArchive) reaction(ctx context.Context, targetID, emoji string, at time.Time) (string, bool) {
	var previous string
	var previousAt int64
	err := a.db.QueryRowContext(ctx,
		`SELECT emoji, at FROM wt_reactions WHERE target_id = ?`, targetID).
		Scan(&previous, &previousAt)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		a.log.Warn("reaction lookup failed", "target", targetID, "err", err)
		return "", false
	}
	if found && at.UnixMilli() <= previousAt {
		return previous, false
	}
	if found && previous == emoji {
		return previous, false
	}
	if emoji == "" {
		// Keep an empty tombstone with its timestamp. Otherwise an older "add"
		// replayed after reconnect could resurrect a reaction that was removed.
		_, err := a.db.ExecContext(ctx, `INSERT INTO wt_reactions (target_id, emoji, at)
			VALUES (?, '', ?) ON CONFLICT(target_id) DO UPDATE SET emoji = '', at = excluded.at`,
			targetID, at.UnixMilli())
		if err != nil {
			a.log.Warn("could not remove reaction", "target", targetID, "err", err)
			return previous, false
		}
		return previous, found && previous != ""
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO wt_reactions (target_id, emoji, at)
		VALUES (?, ?, ?) ON CONFLICT(target_id) DO UPDATE SET emoji = excluded.emoji, at = excluded.at`,
		targetID, emoji, at.UnixMilli()); err != nil {
		a.log.Warn("could not store reaction", "target", targetID, "err", err)
		return previous, false
	}
	return previous, true
}

func (a *messageArchive) prune(ctx context.Context) {
	cutoff := time.Now().Add(-a.retention).Unix()
	queries := []string{
		`DELETE FROM wt_reactions WHERE target_id IN (SELECT id FROM wt_messages WHERE at < ?)`,
		`DELETE FROM wt_message_revisions WHERE id IN (SELECT id FROM wt_messages WHERE at < ?)`,
		`DELETE FROM wt_messages WHERE at < ?`,
	}
	for _, q := range queries {
		if _, err := a.db.ExecContext(ctx, q, cutoff); err != nil {
			a.log.Warn("message archive prune failed", "err", err)
			return
		}
	}
	if _, err := a.db.ExecContext(ctx, `DELETE FROM wt_reactions WHERE at < ?`,
		time.Now().Add(-a.retention).UnixMilli()); err != nil {
		a.log.Warn("reaction tombstone prune failed", "err", err)
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func timeMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func describeArchivedTarget(m archivedMessage, now time.Time) string {
	if m.ID == "" {
		return "un messaggio"
	}
	return m.Kind + " " + whenPhrase(m.At, now)
}

// excerpt keeps activity sensor states well below Home Assistant's 255
// character state limit, while preserving the useful part of deleted/edited
// text. Newlines are flattened because these labels also go to the logbook.
func excerpt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const maxRunes = 100
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes-1]) + "…"
	}
	return s
}
