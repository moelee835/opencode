package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/opencode-ai/opencode/internal/config"
	"github.com/opencode-ai/opencode/internal/db"
	"github.com/opencode-ai/opencode/internal/format"
	"github.com/opencode-ai/opencode/internal/history"
	"github.com/opencode-ai/opencode/internal/llm/agent"
	"github.com/opencode-ai/opencode/internal/llm/models"
	"github.com/opencode-ai/opencode/internal/llm/tools"
	"github.com/opencode-ai/opencode/internal/llm/tools/shell"
	"github.com/opencode-ai/opencode/internal/logging"
	"github.com/opencode-ai/opencode/internal/lsp"
	"github.com/opencode-ai/opencode/internal/message"
	"github.com/opencode-ai/opencode/internal/messaging"
	"github.com/opencode-ai/opencode/internal/permission"
	"github.com/opencode-ai/opencode/internal/session"
	"github.com/opencode-ai/opencode/internal/teams"
	"github.com/opencode-ai/opencode/internal/tui/theme"
)

type App struct {
	Teams       *teams.Store
	TeamRuntime *teams.Runtime
	Mail        *messaging.Service
	Sessions    session.Service
	Messages    message.Service
	History     history.Service
	Permissions permission.Service

	CoderAgent agent.Service

	LSPClients map[string]*lsp.Client

	clientsMutex sync.RWMutex

	watcherCancelFuncs []context.CancelFunc
	cancelFuncsMutex   sync.Mutex
	watcherWG          sync.WaitGroup
}

func New(ctx context.Context, conn *sql.DB) (*App, error) {
	q := db.New(conn)
	sessions := session.NewService(q)
	messages := message.NewService(q)
	files := history.NewService(q, conn)

	app := &App{
		Mail:        messaging.New(conn),
		Sessions:    sessions,
		Messages:    messages,
		History:     files,
		Permissions: permission.NewPermissionService(),
		LSPClients:  make(map[string]*lsp.Client),
	}

	app.Teams = teams.NewStore(conn)
	app.TeamRuntime = teams.NewRuntime(ctx, app.Teams, &teamRunner{app: app, agents: map[string]agent.Service{}})

	// Initialize theme based on configuration
	app.initTheme()

	// Initialize LSP clients in the background
	go app.initLSPClients(ctx)

	var err error
	app.CoderAgent, err = agent.NewAgent(
		config.AgentCoder,
		app.Sessions,
		app.Messages,
		append(agent.CoderAgentTools(
			app.Mail,
			app.Permissions,
			app.Sessions,
			app.Messages,
			app.History,
			app.toolClients(),
		), tools.NewTeamTool(app.Teams, app.TeamRuntime, app.Mail, app.Permissions)),
	)
	if err != nil {
		logging.Error("Failed to create coder agent", err)
		return nil, err
	}

	return app, nil
}

// initTheme sets the application theme based on the configuration
func (app *App) initTheme() {
	cfg := config.Get()
	if cfg == nil || cfg.TUI.Theme == "" {
		return // Use default theme
	}

	// Try to set the theme from config
	err := theme.SetTheme(cfg.TUI.Theme)
	if err != nil {
		logging.Warn("Failed to set theme from config, using default theme", "theme", cfg.TUI.Theme, "error", err)
	} else {
		logging.Debug("Set theme from config", "theme", cfg.TUI.Theme)
	}
}

// RunNonInteractive handles the execution flow when a prompt is provided via CLI flag.
func (a *App) RunNonInteractive(ctx context.Context, prompt string, outputFormat string, quiet bool) error {
	logging.Info("Running in non-interactive mode")
	ctx = context.WithValue(ctx, tools.TeamsDisabledContextKey, true)

	// Start spinner if not in quiet mode
	var spinner *format.Spinner
	if !quiet {
		spinner = format.NewSpinner("Thinking...")
		spinner.Start()
		defer spinner.Stop()
	}

	const maxPromptLengthForTitle = 100
	titlePrefix := "Non-interactive: "
	var titleSuffix string

	if len(prompt) > maxPromptLengthForTitle {
		titleSuffix = prompt[:maxPromptLengthForTitle] + "..."
	} else {
		titleSuffix = prompt
	}
	title := titlePrefix + titleSuffix

	sess, err := a.Sessions.Create(ctx, title)
	if err != nil {
		return fmt.Errorf("failed to create session for non-interactive mode: %w", err)
	}
	logging.Info("Created session for non-interactive run", "session_id", sess.ID)

	// Automatically approve all permission requests for this non-interactive session
	a.Permissions.AutoApproveSession(sess.ID)

	done, err := a.CoderAgent.Run(ctx, sess.ID, prompt)
	if err != nil {
		return fmt.Errorf("failed to start agent processing stream: %w", err)
	}

	result := <-done
	if result.Error != nil {
		if errors.Is(result.Error, context.Canceled) || errors.Is(result.Error, agent.ErrRequestCancelled) {
			logging.Info("Agent processing cancelled", "session_id", sess.ID)
			return nil
		}
		return fmt.Errorf("agent processing failed: %w", result.Error)
	}

	// Stop spinner before printing output
	if !quiet && spinner != nil {
		spinner.Stop()
	}

	// Get the text content from the response
	content := "No content available"
	if result.Message.Content().String() != "" {
		content = result.Message.Content().String()
	}

	fmt.Println(format.FormatOutput(content, outputFormat))

	logging.Info("Non-interactive run completed", "session_id", sess.ID)

	return nil
}

// Shutdown performs a clean shutdown of the application
func (app *App) Shutdown() {
	if app.CoderAgent != nil {
		app.CoderAgent.CancelAll()
	}
	app.TeamRuntime.Close()
	shell.CloseAll()
	// Cancel all watcher goroutines
	app.cancelFuncsMutex.Lock()
	for _, cancel := range app.watcherCancelFuncs {
		cancel()
	}
	app.cancelFuncsMutex.Unlock()
	app.watcherWG.Wait()

	// Perform additional cleanup for LSP clients
	app.clientsMutex.RLock()
	clients := make(map[string]*lsp.Client, len(app.LSPClients))
	maps.Copy(clients, app.LSPClients)
	app.clientsMutex.RUnlock()

	for name, client := range clients {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := client.Shutdown(shutdownCtx); err != nil {
			logging.Error("Failed to shutdown LSP client", "name", name, "error", err)
		}
		cancel()
	}
}

// Teammates inherit the coder model or select an explicit model. Conversation
// histories are independent; all executors share per-session run locks.
type teamRunner struct {
	app    *App
	mu     sync.Mutex
	agents map[string]agent.Service
}

func (r *teamRunner) Busy(id string) bool { return r.app.CoderAgent.IsSessionBusy(id) }
func (r *teamRunner) Run(ctx context.Context, m teams.Member, input string) (string, error) {
	executor := r.app.CoderAgent
	if m.Model != "" {
		r.mu.Lock()
		var err error
		executor = r.agents[m.Model]
		if executor == nil {
			executor, err = agent.NewAgentWithModel(models.ModelID(m.Model), r.app.Sessions, r.app.Messages, append(agent.CoderAgentTools(r.app.Mail, r.app.Permissions, r.app.Sessions, r.app.Messages, r.app.History, r.app.toolClients()), tools.NewTeamTool(r.app.Teams, r.app.TeamRuntime, r.app.Mail, r.app.Permissions)))
			if err == nil {
				r.agents[m.Model] = executor
			}
		}
		r.mu.Unlock()
		if err != nil {
			return "", err
		}
	}
	for {
		done, err := executor.Run(ctx, m.SessionID, input)
		if errors.Is(err, agent.ErrSessionBusy) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}
		if err != nil {
			return "", err
		}
		// Drain completion even after cancellation; releasing a team lease must not
		// permit a replacement runtime while the old executor is still running.
		result := <-done
		if result.Error != nil {
			return "", result.Error
		}
		switch result.Message.FinishReason() {
		case message.FinishReasonCanceled, message.FinishReasonError, message.FinishReasonPermissionDenied, message.FinishReasonMaxTokens:
			return "", fmt.Errorf("agent stopped: %s", result.Message.FinishReason())
		}
		return result.Message.Content().String(), nil
	}
}

func (app *App) toolClients() map[string]*lsp.Client {
	app.clientsMutex.RLock()
	defer app.clientsMutex.RUnlock()
	return maps.Clone(app.LSPClients)
}
