package page

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/opencode-ai/opencode/internal/app"
	"github.com/opencode-ai/opencode/internal/teams"
	"github.com/opencode-ai/opencode/internal/tui/styles"
	"github.com/opencode-ai/opencode/internal/tui/util"
)

var TeamsPage PageID = "teams"

type TeamMemberSelectedMsg struct{ SessionID string }
type teamRefresh struct{}
type teamSnapshot struct {
	data []teams.Snapshot
	err  error
}
type teamsPage struct {
	app                             *app.App
	width, height, teamIndex, index int
	tasks                           bool
	data                            []teams.Snapshot
	err                             error
}

func NewTeamsPage(app *app.App) tea.Model { return &teamsPage{app: app} }
func (p *teamsPage) load() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		all, e := p.app.Teams.List(ctx)
		if e != nil {
			return teamSnapshot{err: e}
		}
		data := []teams.Snapshot{}
		for _, t := range all {
			s, e := p.app.Teams.Snapshot(ctx, t.ID)
			if e != nil {
				return teamSnapshot{err: e}
			}
			data = append(data, s)
		}
		return teamSnapshot{data: data}
	}
}
func (p *teamsPage) Init() tea.Cmd              { return p.load() }
func (p *teamsPage) SetSize(w, h int) tea.Cmd   { p.width = w; p.height = h; return nil }
func (p *teamsPage) GetSize() (int, int)        { return p.width, p.height }
func (p *teamsPage) BindingKeys() []key.Binding { return nil }
func (p *teamsPage) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case teamRefresh:
		return p, p.load()
	case teamSnapshot:
		p.data = m.data
		p.err = m.err
		return p, tea.Tick(time.Second, func(time.Time) tea.Msg { return teamRefresh{} })
	case tea.WindowSizeMsg:
		return p, p.SetSize(m.Width, m.Height)
	case tea.KeyMsg:
		if m.String() == "esc" {
			return p, util.CmdHandler(PageChangeMsg{ID: ChatPage})
		}
		if len(p.data) == 0 {
			return p, nil
		}
		p.teamIndex = util.Clamp(p.teamIndex, 0, len(p.data)-1)
		s := p.data[p.teamIndex]
		switch m.String() {
		case "left":
			p.teamIndex = max(0, p.teamIndex-1)
			p.index = 0
		case "right":
			p.teamIndex = min(len(p.data)-1, p.teamIndex+1)
			p.index = 0
		case "tab":
			p.tasks = !p.tasks
			p.index = 0
		case "up", "k":
			p.index = max(0, p.index-1)
		case "down", "j":
			p.index++
		case "enter":
			if !p.tasks && p.index < len(s.Members) {
				return p, util.CmdHandler(TeamMemberSelectedMsg{s.Members[p.index].SessionID})
			}
		case "p", "r", "s", "a", "x", "y", "R":
			action := map[string]string{"p": "pause", "r": "resume", "s": "stop", "a": "approve", "x": "cancel", "y": "retry", "R": "resume"}[m.String()]
			target := ""
			if m.String() != "R" {
				if p.tasks {
					if p.index >= len(s.Tasks) {
						return p, nil
					}
					target = s.Tasks[p.index].ID
				} else {
					if p.index >= len(s.Members) {
						return p, nil
					}
					target = s.Members[p.index].SessionID
				}
			}
			return p, func() tea.Msg {
				e := p.app.Teams.Control(context.Background(), s.Team.ID, s.Team.Lead, target, action)
				if e == nil && action == "resume" {
					e = p.app.TeamRuntime.Start(context.Background(), s.Team.ID)
				}
				if e != nil {
					return util.InfoMsg{Type: util.InfoTypeError, Msg: e.Error()}
				}
				return teamRefresh{}
			}
		}
	}
	return p, nil
}
func (p *teamsPage) View() string {
	lines := []string{"Agent Teams", "←/→ team • Tab members/tasks • ↑/↓ select • Enter open conversation • Esc back", "p pause • r resume • s stop member • a approve / x cancel / y retry task • R start/resume team", ""}
	if p.err != nil {
		lines = append(lines, p.err.Error())
	} else if len(p.data) == 0 {
		lines = append(lines, "No teams. Ask the agent to create a team, or use opencode teams create.")
	} else {
		s := p.data[util.Clamp(p.teamIndex, 0, len(p.data)-1)]
		lines = append(lines, fmt.Sprintf("%s [%s]  %d teammates · %d tasks", s.Team.Name, s.Team.Status, len(s.Members)-1, len(s.Tasks)))
		rows := []string{}
		if p.tasks {
			for _, t := range s.Tasks {
				rows = append(rows, fmt.Sprintf("[%s] %s · %s · dependencies %d", t.Status, t.Title, t.Assignee, len(t.Dependencies)))
			}
		} else {
			for _, m := range s.Members {
				rows = append(rows, fmt.Sprintf("%s (%s) [%s] turns %d/%d · %s", m.Name, m.Role, m.Status, m.Turns, m.MaxTurns, m.SessionID))
			}
		}
		selected := util.Clamp(p.index, 0, max(0, len(rows)-1))
		start := max(0, selected-max(1, p.height-9)+1)
		for i := start; i < len(rows) && i < start+max(1, p.height-9); i++ {
			prefix := "  "
			if i == selected {
				prefix = "> "
			}
			lines = append(lines, prefix+rows[i])
		}
	}
	return styles.BaseStyle().Width(p.width).Height(p.height).Render(lipgloss.NewStyle().MaxWidth(max(1, p.width)).Render(strings.Join(lines, "\n")))
}
