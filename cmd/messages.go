package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/signal"

	"github.com/opencode-ai/opencode/internal/db"
	"github.com/opencode-ai/opencode/internal/messaging"
	"github.com/opencode-ai/opencode/internal/session"
	"github.com/spf13/cobra"
)

func newMessagesCommand() *cobra.Command {
	var directory string
	parent := &cobra.Command{Use: "messages", Short: "Exchange durable messages between sessions in one project"}
	parent.PersistentFlags().StringVar(&directory, "data-dir", ".opencode", "Project data directory containing opencode.db")
	withService := func(run func(*cobra.Command, *messaging.Service, session.Service) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			conn, err := db.Open(directory)
			if err != nil {
				return err
			}
			defer conn.Close()
			return run(cmd, messaging.New(conn), session.NewService(db.New(conn)))
		}
	}
	encode := func(cmd *cobra.Command, value any) error { return json.NewEncoder(cmd.OutOrStdout()).Encode(value) }
	parent.AddCommand(&cobra.Command{Use: "sessions", Short: "List available top-level sessions", Args: cobra.NoArgs, RunE: withService(func(cmd *cobra.Command, mail *messaging.Service, sessions session.Service) error {
		items, err := sessions.List(cmd.Context())
		if err != nil {
			return err
		}
		return encode(cmd, items)
	})})
	var from, to, body, key string
	send := &cobra.Command{Use: "send", Short: "Send a message; reuse --key to retry safely", Args: cobra.NoArgs, RunE: withService(func(cmd *cobra.Command, mail *messaging.Service, _ session.Service) error {
		m, err := mail.Send(cmd.Context(), from, to, body, key)
		if err != nil {
			return err
		}
		return encode(cmd, m)
	})}
	send.Flags().StringVar(&from, "from", "", "Sender session ID")
	send.Flags().StringVar(&to, "to", "", "Recipient session ID")
	send.Flags().StringVar(&body, "body", "", "Message text (up to 32 KiB)")
	send.Flags().StringVar(&key, "key", "", "Idempotency key, scoped to sender")
	for _, name := range []string{"from", "to", "body"} {
		_ = send.MarkFlagRequired(name)
	}
	parent.AddCommand(send)
	for _, watching := range []bool{false, true} {
		var recipient string
		var after int64
		var unread bool
		name, short := "inbox", "List a page of up to 100 messages without marking them read"
		if watching {
			name, short = "watch", "Stream messages as JSON lines; resume with --after SEQ"
		}
		command := &cobra.Command{Use: name, Short: short, Args: cobra.NoArgs, RunE: withService(func(cmd *cobra.Command, mail *messaging.Service, sessions session.Service) error {
			if recipient == "" {
				return errors.New("session ID is required")
			}
			if _, err := sessions.Get(cmd.Context(), recipient); err != nil {
				return err
			}
			if watching {
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
				defer stop()
				err := mail.Watch(ctx, recipient, after, func(m messaging.Message) error { return encode(cmd, m) })
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}
			items, err := mail.List(cmd.Context(), recipient, after, unread)
			if err != nil {
				return err
			}
			return encode(cmd, items)
		})}
		command.Flags().StringVar(&recipient, "session", "", "Recipient session ID")
		command.Flags().Int64Var(&after, "after", 0, "Only messages after this sequence number")
		if !watching {
			command.Flags().BoolVar(&unread, "unread", false, "Only unread messages")
		}
		_ = command.MarkFlagRequired("session")
		parent.AddCommand(command)
	}
	var recipient, id string
	read := &cobra.Command{Use: "read", Short: "Explicitly mark one received message as read", Args: cobra.NoArgs, RunE: withService(func(cmd *cobra.Command, mail *messaging.Service, _ session.Service) error {
		if err := mail.MarkRead(cmd.Context(), recipient, id); err != nil {
			return err
		}
		return encode(cmd, map[string]string{"id": id, "status": "read"})
	})}
	read.Flags().StringVar(&recipient, "session", "", "Recipient session ID")
	read.Flags().StringVar(&id, "id", "", "Message ID")
	_ = read.MarkFlagRequired("session")
	_ = read.MarkFlagRequired("id")
	parent.AddCommand(read)
	return parent
}

func init() { rootCmd.AddCommand(newMessagesCommand()) }
