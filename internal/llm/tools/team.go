package tools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/opencode-ai/opencode/internal/llm/models"
	"github.com/opencode-ai/opencode/internal/messaging"
	"github.com/opencode-ai/opencode/internal/permission"
	"github.com/opencode-ai/opencode/internal/teams"
)

type teamContextKey string

const TeamsDisabledContextKey teamContextKey = "teams_disabled"

type TeamStarter interface {
	Start(context.Context, string) error
}
type teamTool struct {
	store       *teams.Store
	runtime     TeamStarter
	mail        *messaging.Service
	permissions permission.Service
}

func NewTeamTool(store *teams.Store, runtime TeamStarter, mail *messaging.Service, p permission.Service) BaseTool {
	return &teamTool{store, runtime, mail, p}
}
func (t *teamTool) Info() ToolInfo {
	return ToolInfo{Name: "team", Description: "Manage an agent team. create makes you lead; spawn creates a named teammate and starts its initial task. Workers run concurrently with independent conversation histories; incoming messages automatically execute after the current turn. task creates work with optional dependencies and assignee. status shows team, members, tasks, results. message sends by teammate name (target) or broadcasts with target=all. pause/stop/resume control a member (target session ID) or entire team (empty target). approve/retry/cancel control a task ID. assign moves a non-running task (target) to assignee. Only the lead may spawn/control. No automatic replay of failed work. Avoid overlapping file edits; give workers explicit file ownership. Returning a final answer completes the assigned task and automatically reports to the lead. Do not repeatedly acknowledge completion notifications.", Parameters: map[string]any{
		"action": map[string]any{"type": "string", "enum": []string{"create", "spawn", "task", "status", "message", "pause", "stop", "resume", "approve", "retry", "cancel", "assign"}},
		"team":   map[string]any{"type": "string", "description": "Team ID; inferred from current membership when omitted"},
		"model":  map[string]any{"type": "string", "description": "Optional supported model ID for this teammate"},
		"name":   map[string]any{"type": "string", "description": "Team/teammate name"},
		"prompt": map[string]any{"type": "string", "description": "Initial role/task instructions or message body"},
		"title":  map[string]any{"type": "string"}, "target": map[string]any{"type": "string", "description": "Teammate name for message, session ID for control, task ID for approve/retry/cancel"},
		"assignee":     map[string]any{"type": "string", "description": "Worker session ID; omit to let an idle worker claim"},
		"dependencies": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"approval":     map[string]any{"type": "boolean", "description": "Require lead approval before executing this task"},
		"max_turns":    map[string]any{"type": "integer", "description": "Automatic turns per teammate, default 30; resume resets budget"},
	}, Required: []string{"action"}}
}
func (t *teamTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	if disabled, _ := ctx.Value(TeamsDisabledContextKey).(bool); disabled {
		return NewTextErrorResponse("Agent teams require the interactive TUI or opencode teams run; one-shot -p mode cannot host teammates"), nil
	}
	var p struct {
		Action, Team, Name, Prompt, Title, Target, Assignee, Model string
		Dependencies                                               []string
		Approval                                                   bool
		MaxTurns                                                   int `json:"max_turns"`
	}
	if e := json.Unmarshal([]byte(call.Input), &p); e != nil {
		return NewTextErrorResponse(e.Error()), nil
	}
	self, _ := GetContextValues(ctx)
	if self == "" {
		return NewTextErrorResponse("session context required"), nil
	}
	var value any
	var err error
	if p.Action == "create" {
		if !t.permissions.Request(permission.CreatePermissionRequest{Context: ctx, SessionID: self, ToolName: "team", Action: "create", Description: "Create and run an agent team", Params: p}) {
			return NewTextErrorResponse("permission denied"), nil
		}
		team, e := t.store.Create(ctx, p.Name, self)
		err = e
		value = team
		if e == nil {
			err = t.runtime.Start(ctx, team.ID)
		}
	} else {
		if p.Team == "" {
			m, e := t.store.Member(ctx, self)
			if e != nil {
				return NewTextErrorResponse("create or join a team first"), nil
			}
			p.Team = m.TeamID
		}
		team, e := t.store.Get(ctx, p.Team)
		if e != nil {
			return NewTextErrorResponse(e.Error()), nil
		}
		p.Team = team.ID
		if e = t.store.Authorize(ctx, p.Team, self, false); e != nil {
			return NewTextErrorResponse(e.Error()), nil
		}
		switch p.Action {
		case "status":
			value, err = t.store.Snapshot(ctx, p.Team)
		case "spawn":
			if p.MaxTurns == 0 {
				p.MaxTurns = 30
			}
			if p.Model != "" {
				if _, ok := models.SupportedModels[models.ModelID(p.Model)]; !ok {
					return NewTextErrorResponse("unsupported model"), nil
				}
			}
			value, err = t.store.AddMemberWithModel(ctx, p.Team, self, p.Name, p.Prompt, p.Model, p.MaxTurns)
		case "task":
			value, err = t.store.AddTask(ctx, p.Team, self, p.Title, p.Prompt, p.Assignee, p.Dependencies, p.Approval)
		case "message":
			if !t.permissions.Request(permission.CreatePermissionRequest{Context: ctx, SessionID: self, ToolName: "team", Action: "message:" + p.Team + ":" + p.Target, Description: "Send team message", Params: p}) {
				return NewTextErrorResponse("permission denied"), nil
			}
			members, e := t.store.Members(ctx, p.Team)
			err = e
			sent := []messaging.Message{}
			if e == nil {
				for _, m := range members {
					if m.SessionID != self && (p.Target == "all" || p.Target == m.Name || p.Target == m.SessionID) {
						msg, e := t.mail.Send(ctx, self, m.SessionID, p.Prompt, call.ID+":"+m.SessionID)
						if e != nil {
							err = e
							break
						}
						sent = append(sent, msg)
					}
				}
				if len(sent) == 0 && err == nil {
					err = errors.New("recipient not found")
				}
			}
			value = sent
		case "assign":
			err = t.store.Assign(ctx, p.Team, self, p.Target, p.Assignee)
			value = map[string]string{"status": "assigned"}
		case "pause", "stop", "resume", "approve", "retry", "cancel":
			err = t.store.Control(ctx, p.Team, self, p.Target, p.Action)
			value = map[string]string{"status": p.Action}
			if err == nil && p.Action == "resume" {
				err = t.runtime.Start(ctx, p.Team)
			}
		default:
			err = errors.New("unknown team action")
		}
	}
	if err != nil {
		return NewTextErrorResponse(err.Error()), nil
	}
	data, e := json.Marshal(value)
	if e != nil {
		return ToolResponse{}, e
	}
	return NewTextResponse(string(data)), nil
}
