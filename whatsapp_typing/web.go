package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web_ui.html
var archiveHTML []byte

type ArchiveWeb struct {
	db      *sql.DB
	contact string
	log     *slog.Logger
}

type archiveRow struct {
	ID        string            `json:"id"`
	At        time.Time         `json:"at"`
	Kind      string            `json:"kind"`
	Text      string            `json:"text"`
	FromMe    bool              `json:"from_me"`
	QuotedID  string            `json:"quoted_id,omitempty"`
	Ephemeral bool              `json:"ephemeral,omitempty"`
	ViewOnce  bool              `json:"view_once,omitempty"`
	Forwarded bool              `json:"forwarded,omitempty"`
	Duration  int               `json:"duration,omitempty"`
	EditedAt  *time.Time        `json:"edited_at,omitempty"`
	DeletedAt *time.Time        `json:"deleted_at,omitempty"`
	Revisions []archiveRevision `json:"revisions,omitempty"`
}

type archiveRevision struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

type archiveResponse struct {
	Contact  string       `json:"contact"`
	Messages []archiveRow `json:"messages"`
}

func NewArchiveWeb(db *sql.DB, contact string, log *slog.Logger) *ArchiveWeb {
	return &ArchiveWeb{db: db, contact: contact, log: log}
}

func (w *ArchiveWeb) Start(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.index)
	mux.HandleFunc("/api/messages", w.messages)
	srv := &http.Server{
		Addr:              addr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
	}
	go func() {
		w.log.Info("archive web interface listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			w.log.Error("archive web interface stopped", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Header().Set("Referrer-Policy", "no-referrer")
		rw.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
		next.ServeHTTP(rw, r)
	})
}

func (w *ArchiveWeb) index(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Write(archiveHTML)
}

func (w *ArchiveWeb) messages(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 1000 {
			limit = n
		}
	}
	rows, err := w.queryMessages(r.Context(), r.URL.Query().Get("filter"), r.URL.Query().Get("q"), limit)
	if err != nil {
		w.log.Error("archive query failed", "err", err)
		http.Error(rw, "archive query failed", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(rw).Encode(archiveResponse{Contact: w.contact, Messages: rows})
}

func (w *ArchiveWeb) queryMessages(ctx context.Context, filter, search string, limit int) ([]archiveRow, error) {
	where := []string{"1=1"}
	args := []any{}
	switch filter {
	case "received":
		where = append(where, "from_me = 0")
	case "sent":
		where = append(where, "from_me = 1")
	case "deleted":
		where = append(where, "deleted_at > 0")
	case "edited":
		where = append(where, "edited_at > 0")
	}
	if search = strings.TrimSpace(search); search != "" {
		where = append(where, "text LIKE ? ESCAPE '\\'")
		escaped := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(search)
		args = append(args, "%"+escaped+"%")
	}
	args = append(args, limit)
	query := `SELECT id, at, kind, text, from_me, quoted_id, ephemeral, view_once,
		forwarded, duration, edited_at, deleted_at FROM wt_messages WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY at DESC LIMIT ?`
	dbRows, err := w.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer dbRows.Close()
	result := make([]archiveRow, 0)
	for dbRows.Next() {
		var row archiveRow
		var at, edited, deleted int64
		var fromMe, ephemeral, viewOnce, forwarded int
		if err := dbRows.Scan(&row.ID, &at, &row.Kind, &row.Text, &fromMe, &row.QuotedID,
			&ephemeral, &viewOnce, &forwarded, &row.Duration, &edited, &deleted); err != nil {
			return nil, err
		}
		row.At = time.Unix(at, 0)
		row.FromMe = fromMe != 0
		row.Ephemeral = ephemeral != 0
		row.ViewOnce = viewOnce != 0
		row.Forwarded = forwarded != 0
		if edited > 0 {
			t := time.UnixMilli(edited)
			row.EditedAt = &t
		}
		if deleted > 0 {
			t := time.UnixMilli(deleted)
			row.DeletedAt = &t
		}
		result = append(result, row)
	}
	if err := dbRows.Err(); err != nil {
		return nil, err
	}
	for i := range result {
		revisions, err := w.queryRevisions(ctx, result[i].ID)
		if err != nil {
			return nil, fmt.Errorf("revisions for %s: %w", result[i].ID, err)
		}
		result[i].Revisions = revisions
	}
	return result, nil
}

func (w *ArchiveWeb) queryRevisions(ctx context.Context, id string) ([]archiveRevision, error) {
	rows, err := w.db.QueryContext(ctx,
		`SELECT at, kind, text FROM wt_message_revisions WHERE id = ? ORDER BY at DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []archiveRevision
	for rows.Next() {
		var revision archiveRevision
		var at int64
		if err := rows.Scan(&at, &revision.Kind, &revision.Text); err != nil {
			return nil, err
		}
		revision.At = time.UnixMilli(at)
		result = append(result, revision)
	}
	return result, rows.Err()
}
