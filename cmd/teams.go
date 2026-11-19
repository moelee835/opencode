package cmd

import (
	"encoding/json"
	"os"
	"os/signal"
	"time"

	"github.com/opencode-ai/opencode/internal/app"
	"github.com/opencode-ai/opencode/internal/config"
	"github.com/opencode-ai/opencode/internal/db"
	"github.com/opencode-ai/opencode/internal/session"
	"github.com/opencode-ai/opencode/internal/teams"
	"github.com/spf13/cobra"
)

func newTeamsCommand() *cobra.Command {
	var dir string
	root := &cobra.Command{Use: "teams", Short: "Create, run, inspect and control agent teams"}
	root.PersistentFlags().StringVar(&dir, "data-dir", ".opencode", "Project database directory")
	with := func(fn func(*cobra.Command, []string, *teams.Store) error) func(*cobra.Command, []string) error {
		return func(c *cobra.Command, args []string) error {
			conn, e := db.Open(dir)
			if e != nil {
				return e
			}
			defer conn.Close()
			return fn(c, args, teams.NewStore(conn))
		}
	}
	output := func(c *cobra.Command, v any) error { return json.NewEncoder(c.OutOrStdout()).Encode(v) }
	root.AddCommand(&cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: with(func(c *cobra.Command, _ []string, s *teams.Store) error {
		v, e := s.List(c.Context())
		if e != nil {
			return e
		}
		return output(c, v)
	})})
	var lead string
	create := &cobra.Command{Use: "create NAME", Args: cobra.ExactArgs(1), RunE: with(func(c *cobra.Command, args []string, s *teams.Store) error {
		if lead == "" {
			v, e := session.NewService(db.New(s.Conn)).Create(c.Context(), args[0]+" / lead")
			if e != nil {
				return e
			}
			lead = v.ID
		}
		v, e := s.Create(c.Context(), args[0], lead)
		if e != nil {
			return e
		}
		return output(c, v)
	})}
	create.Flags().StringVar(&lead, "lead", "", "Existing lead session; otherwise create one")
	root.AddCommand(create)
	var instruction, model string
	var maxTurns int
	spawn := &cobra.Command{Use: "spawn TEAM NAME", Args: cobra.ExactArgs(2), RunE: with(func(c *cobra.Command, args []string, s *teams.Store) error {
		t, e := s.Get(c.Context(), args[0])
		if e != nil {
			return e
		}
		m, e := s.AddMemberWithModel(c.Context(), t.ID, t.Lead, args[1], instruction, model, maxTurns)
		if e != nil {
			return e
		}
		return output(c, m)
	})}
	spawn.Flags().StringVar(&model, "model", "", "Optional supported model ID (defaults to coder model)")
	spawn.Flags().StringVar(&instruction, "prompt", "", "Role and initial task instructions")
	spawn.Flags().IntVar(&maxTurns, "max-turns", 30, "Automatic turn budget (1-1000)")
	_ = spawn.MarkFlagRequired("prompt")
	root.AddCommand(spawn)
	var prompt, title, assignee string
	var deps []string
	var approval bool
	task := &cobra.Command{Use: "task TEAM", Args: cobra.ExactArgs(1), RunE: with(func(c *cobra.Command, args []string, s *teams.Store) error {
		t, e := s.Get(c.Context(), args[0])
		if e != nil {
			return e
		}
		v, e := s.AddTask(c.Context(), t.ID, t.Lead, title, prompt, assignee, deps, approval)
		if e != nil {
			return e
		}
		return output(c, v)
	})}
	task.Flags().StringVar(&title, "title", "", "Task title")
	task.Flags().StringVar(&prompt, "prompt", "", "Task instructions")
	task.Flags().StringVar(&assignee, "assignee", "", "Worker session ID (otherwise claimed by an idle worker)")
	task.Flags().StringSliceVar(&deps, "depends-on", nil, "Dependency task IDs")
	task.Flags().BoolVar(&approval, "require-approval", false, "Block execution until lead approval")
	_ = task.MarkFlagRequired("title")
	_ = task.MarkFlagRequired("prompt")
	root.AddCommand(task)
	for _, watch := range []bool{false, true} {
		name := "status"
		if watch {
			name = "watch"
		}
		root.AddCommand(&cobra.Command{Use: name + " TEAM", Args: cobra.ExactArgs(1), RunE: with(func(c *cobra.Command, args []string, s *teams.Store) error {
			ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt)
			defer stop()
			for {
				v, e := s.Snapshot(ctx, args[0])
				if e != nil {
					return e
				}
				if e = output(c, v); e != nil {
					return e
				}
				if !watch {
					return nil
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(time.Second):
				}
			}
		})})
	}
	for _, action := range []string{"pause", "resume", "stop", "approve", "retry", "cancel"} {
		root.AddCommand(&cobra.Command{Use: action + " TEAM [TARGET]", Args: cobra.RangeArgs(1, 2), RunE: with(func(c *cobra.Command, args []string, s *teams.Store) error {
			t, e := s.Get(c.Context(), args[0])
			if e != nil {
				return e
			}
			target := ""
			if len(args) == 2 {
				target = args[1]
			}
			if e = s.Control(c.Context(), t.ID, t.Lead, target, action); e != nil {
				return e
			}
			return output(c, map[string]string{"status": action})
		})})
	}
	root.AddCommand(&cobra.Command{Use: "assign TEAM TASK WORKER_SESSION", Args: cobra.ExactArgs(3), RunE: with(func(c *cobra.Command, args []string, s *teams.Store) error {
		t, e := s.Get(c.Context(), args[0])
		if e != nil {
			return e
		}
		return s.Assign(c.Context(), t.ID, t.Lead, args[1], args[2])
	})})
	var body string
	tell := &cobra.Command{Use: "tell TEAM MEMBER", Short: "Queue a human instruction for a teammate by name or session ID", Args: cobra.ExactArgs(2), RunE: with(func(c *cobra.Command, args []string, s *teams.Store) error {
		m, e := s.Tell(c.Context(), args[0], args[1], body)
		if e != nil {
			return e
		}
		return output(c, m)
	})}
	tell.Flags().StringVar(&body, "body", "", "Instruction text")
	_ = tell.MarkFlagRequired("body")
	root.AddCommand(tell)
	var allow bool
	run := &cobra.Command{Use: "run TEAM", Short: "Host a team until Ctrl+C or team stop; permission-requiring tools are denied unless --allow-tools", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		cwd, e := os.Getwd()
		if e != nil {
			return e
		}
		cfg, e := config.Load(cwd, false)
		if e != nil {
			return e
		}
		cfg.Data.Directory = dir
		conn, e := db.Open(dir)
		if e != nil {
			return e
		}
		defer conn.Close()
		ctx, cancel := signal.NotifyContext(c.Context(), os.Interrupt)
		defer cancel()
		application, e := app.New(ctx, conn)
		if e != nil {
			return e
		}
		defer application.Shutdown()
		approvals := application.Permissions.Subscribe(ctx)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case event, ok := <-approvals:
					if !ok {
						return
					}
					if allow {
						application.Permissions.Grant(event.Payload)
					} else {
						application.Permissions.Deny(event.Payload)
					}
				}
			}
		}()
		t, e := application.Teams.Get(ctx, args[0])
		if e != nil {
			return e
		}
		if e = application.TeamRuntime.Start(ctx, t.ID); e != nil {
			return e
		}
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
				t, e = application.Teams.Get(ctx, t.ID)
				if e != nil {
					return e
				}
				if t.Status == "stopped" {
					return nil
				}
			}
		}
	}}
	run.Flags().BoolVar(&allow, "allow-tools", false, "Approve permission-requiring tools for this explicitly started team runtime")
	root.AddCommand(run)
	return root
}
func init() { rootCmd.AddCommand(newTeamsCommand()) }
