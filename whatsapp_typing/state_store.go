package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// stateStore keeps the last known values across restarts.
//
// Without it every restart wiped the tracker's memory: the sensors published
// "unknown", and the events WhatsApp replays on reconnect were then treated as
// brand new, re-announcing a reaction that had already happened. The timestamp
// guards in the tracker only work if they survive the restart, which is what
// this table is for.
type stateStore struct {
	db  *sql.DB
	log logger
}

// durableState is the part of the tracker worth surviving a restart: the last
// known values and today's counters. Live typing state is deliberately absent —
// after a restart nobody is typing.
type durableState struct {
	Day string `json:"day"`

	LastTypingAt  time.Time `json:"last_typing_at"`
	LastDuration  int       `json:"last_duration"`
	SessionsToday int       `json:"sessions_today"`
	SecondsToday  int       `json:"seconds_today"`
	PausesToday   int       `json:"pauses_today"`
	RestartsToday int       `json:"restarts_today"`

	LastDelivered time.Time `json:"last_delivered"`
	LastRead      time.Time `json:"last_read"`
	LastPlayed    time.Time `json:"last_played"`
	ReadsToday    int       `json:"reads_today"`
	ReadTarget    string    `json:"read_target"`
	PlayedTarget  string    `json:"played_target"`

	LastMessage     time.Time `json:"last_message"`
	LastMessageType string    `json:"last_message_type"`
	MessagesToday   int       `json:"messages_today"`

	PresenceKnown bool      `json:"presence_known"`
	Online        bool      `json:"online"`
	LastSeen      time.Time `json:"last_seen"`
	LastPresence  time.Time `json:"last_presence"`

	LastReaction       time.Time `json:"last_reaction"`
	LastReactionEmoji  string    `json:"last_reaction_emoji"`
	LastReactionTarget string    `json:"last_reaction_target"`
	ReactionSeen       bool      `json:"reaction_seen"`
	LastEdit           time.Time `json:"last_edit"`
	LastDelete         time.Time `json:"last_delete"`

	// The last event is kept for display only. EventSeq deliberately is not:
	// restoring it would make the publisher fire the restored event on startup
	// and notify you about something that happened before the restart.
	LastEventType string    `json:"last_event_type"`
	LastEventText string    `json:"last_event_text"`
	LastEventTime time.Time `json:"last_event_time"`

	Timeline []TimelineEntry `json:"timeline"`
}

func newStateStore(ctx context.Context, db *sql.DB, log logger) (*stateStore, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS wt_state (
		id       INTEGER PRIMARY KEY CHECK (id = 1),
		data     TEXT NOT NULL,
		saved_at INTEGER NOT NULL
	)`)
	if err != nil {
		return nil, err
	}
	return &stateStore{db: db, log: log}, nil
}

func (s *stateStore) load(ctx context.Context) (durableState, bool) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM wt_state WHERE id = 1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return durableState{}, false
	}
	if err != nil {
		s.log.Warn("could not read saved state", "err", err)
		return durableState{}, false
	}
	var st durableState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		// A corrupt row must not keep the add-on from starting: start fresh.
		s.log.Warn("saved state is unreadable, starting from scratch", "err", err)
		return durableState{}, false
	}
	return st, true
}

func (s *stateStore) save(ctx context.Context, st durableState) {
	raw, err := json.Marshal(st)
	if err != nil {
		s.log.Warn("could not encode state", "err", err)
		return
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO wt_state (id, data, saved_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET data = excluded.data, saved_at = excluded.saved_at`,
		string(raw), time.Now().Unix())
	if err != nil {
		s.log.Warn("could not save state", "err", err)
	}
}
