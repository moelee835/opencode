// Package messaging provides a durable mailbox shared by local OpenCode processes.
// Mail never starts an agent or modifies its conversation history implicitly.
package messaging

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const MaxBodyBytes = 32 * 1024
const PollInterval = 250 * time.Millisecond

type Message struct {
	Seq         int64  `json:"seq"`
	ID          string `json:"id"`
	SenderID    string `json:"sender_id"`
	RecipientID string `json:"recipient_id"`
	Body        string `json:"body"`
	CreatedAt   int64  `json:"created_at"`
	ReadAt      *int64 `json:"read_at,omitempty"`
}

type Service struct{ conn *sql.DB }

func New(conn *sql.DB) *Service { return &Service{conn: conn} }

const columns = "seq, id, sender_id, recipient_id, body, created_at, read_at"

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Message, error) {
	var m Message
	err := row.Scan(&m.Seq, &m.ID, &m.SenderID, &m.RecipientID, &m.Body, &m.CreatedAt, &m.ReadAt)
	return m, err
}

// Send is idempotent for a sender and request key. Reusing a key with different
// content is an error. A successful return means the message is committed.
func (s *Service) Send(ctx context.Context, from, to, body, key string) (Message, error) {
	if from == "" || to == "" || from == to {
		return Message{}, errors.New("distinct sender and recipient sessions are required")
	}
	if !utf8.ValidString(body) || strings.TrimSpace(body) == "" || len(body) > MaxBodyBytes {
		return Message{}, fmt.Errorf("body must be valid UTF-8 and contain 1–%d bytes of nonblank text", MaxBodyBytes)
	}
	if key == "" {
		key = uuid.NewString()
	}
	if len(key) > 256 {
		return Message{}, errors.New("request key exceeds 256 bytes")
	}
	_, err := s.conn.ExecContext(ctx, `INSERT INTO session_mail(id,sender_id,recipient_id,body,request_key,created_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(sender_id,request_key) DO NOTHING`, uuid.NewString(), from, to, body, key, time.Now().UnixMilli())
	if err != nil {
		return Message{}, fmt.Errorf("send message (both sessions must exist): %w", err)
	}
	m, err := scan(s.conn.QueryRowContext(ctx, "SELECT "+columns+" FROM session_mail WHERE sender_id=? AND request_key=?", from, key))
	if err != nil {
		return Message{}, err
	}
	if m.RecipientID != to || m.Body != body {
		return Message{}, errors.New("request key already used with different content")
	}
	return m, nil
}

// List returns a bounded page in durable commit order. Reading does not acknowledge.
// An empty recipient selects all mail for project-wide notification consumers.
func (s *Service) List(ctx context.Context, recipient string, after int64, unread bool) ([]Message, error) {
	if after < 0 {
		return nil, errors.New("after must be nonnegative")
	}
	query := "SELECT " + columns + " FROM session_mail WHERE seq>?"
	args := []any{after}
	if recipient != "" {
		query += " AND recipient_id=?"
		args = append(args, recipient)
	}
	if unread {
		query += " AND read_at IS NULL"
	}
	rows, err := s.conn.QueryContext(ctx, query+" ORDER BY seq LIMIT 100", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Message, 0)
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

func (s *Service) MarkRead(ctx context.Context, recipient, id string) error {
	result, err := s.conn.ExecContext(ctx, "UPDATE session_mail SET read_at=COALESCE(read_at,?) WHERE recipient_id=? AND id=?", time.Now().UnixMilli(), recipient, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}

func (s *Service) Latest(ctx context.Context) (int64, error) {
	var seq int64
	err := s.conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM session_mail").Scan(&seq)
	return seq, err
}

// Watch synchronously delivers pages without dropping messages under backpressure.
// The caller persists the last successfully handled seq for reconnect/replay.
func (s *Service) Watch(ctx context.Context, recipient string, after int64, handle func(Message) error) error {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := s.List(ctx, recipient, after, false)
		if err != nil {
			return err
		}
		for _, m := range page {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := handle(m); err != nil {
				return err
			}
			after = m.Seq
		}
		if len(page) == 100 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
