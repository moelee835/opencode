package page

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/opencode-ai/opencode/internal/teams"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTeamDashboardNavigation(t *testing.T) {
	p := &teamsPage{width: 120, height: 25, data: []teams.Snapshot{{Team: teams.Team{Name: "Review", Status: "active"}, Members: []teams.Member{{Name: "lead", Role: "lead", Status: "idle", SessionID: "lead-id"}, {Name: "worker", Role: "worker", Status: "running", SessionID: "worker-id", MaxTurns: 30}}, Tasks: []teams.Task{{Title: "Check changes", Status: "awaiting_approval"}}}}}
	require.Contains(t, p.View(), "worker (worker) [running]")
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyDown})
	require.Nil(t, cmd)
	_, cmd = p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, TeamMemberSelectedMsg{SessionID: "worker-id"}, cmd())
	p.Update(tea.KeyMsg{Type: tea.KeyTab})
	require.Contains(t, p.View(), "Check changes")
	_, cmd = p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, PageChangeMsg{ID: ChatPage}, cmd())
}
