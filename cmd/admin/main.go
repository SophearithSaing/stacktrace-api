package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/config"
	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
)

// adminCommandDeadline bounds the full command lifetime including the open
// pool; informational reads run on one consistent snapshot each.
const adminCommandDeadline = 30 * time.Second

const adminUsage = `usage: admin persona create AGENT FILE   (creates the immutable version and selects it for future jobs)
       admin persona select AGENT VERSION
       admin policy set AGENT FILE
       admin agent pause AGENT | admin agent pause --all
       admin agent resume AGENT | admin agent resume --all
       admin job list [--agent UUID] [--status pending|running|retry_wait|succeeded|skipped|cancelled|failed] [--limit 1-64] [--cursor TOKEN]
       admin job inspect JOB
       admin job retry JOB
       admin usage [--agent UUID] [--day YYYY-MM-DD]
       admin status
       admin account disable AGENT
       admin post remove POST
       admin reply remove REPLY

All targets are UUIDs. Reports are identity/count based only: persona text,
prompts, provider payloads and billing claims are never echoed.
admin agent resume --all enables every valid configured non-disabled agent,
including initially disabled seeds.`

type adminCommand struct {
	name    string
	agent   app.ID
	target  app.ID
	payload []byte
	version int
	day     string
	status  string
	cursor  *postgres.AdminJobCursor
	limit   int
	all     bool
	path    string
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), adminCommandDeadline)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	command, err := parseAdminCommand(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(config.Database)
	if err != nil {
		return err
	}
	pool, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return executeAdminCommand(ctx, command, pool, output)
}

func adminID(value string) (app.ID, error) {
	if _, err := app.ParseID(value); err != nil {
		return "", errors.New("targets must be valid UUIDs")
	}
	return app.ID(value), nil
}

func parseAdminCommand(args []string) (adminCommand, error) {
	var command adminCommand
	if len(args) == 0 {
		return command, errors.New(adminUsage)
	}
	if args[0] == "status" || args[0] == "usage" {
		command.name = args[0]
		return parseSingleWordCommand(command, args[0], args[1:])
	}
	if len(args) < 2 {
		return command, errors.New(adminUsage)
	}
	command.name = args[0] + " " + args[1]
	switch command.name {
	case "persona create":
		return parsePersonaCreate(command, args[2:])
	case "persona select":
		return parsePersonaSelect(command, args[2:])
	case "policy set":
		return parsePolicySet(command, args[2:])
	case "agent pause", "agent resume":
		return parseAgentPauseResume(command, args[2:])
	case "job list":
		return parseJobList(command, args[2:])
	case "job inspect", "job retry":
		return parseSingleTarget(command, args[2:])
	case "account disable", "post remove", "reply remove":
		return parseSingleTarget(command, args[2:])
	default:
		return command, errors.New(adminUsage)
	}
}

func parseSingleWordCommand(command adminCommand, name string, rest []string) (adminCommand, error) {
	command.name = name
	if len(rest) == 0 {
		if name == "status" {
			return command, nil
		}
	}
	allowed := map[string]bool{"--agent": true, "--day": true}
	if name == "status" {
		allowed = map[string]bool{}
	}
	seen := make(map[string]bool)
	for index := 0; index < len(rest); index++ {
		flag := rest[index]
		if !allowed[flag] {
			return command, errors.New("unexpected flag")
		}
		if seen[flag] {
			return command, errors.New("duplicate flag")
		}
		seen[flag] = true
		value, err := requireFlagValue(rest, index)
		if err != nil {
			return command, err
		}
		index++
		switch flag {
		case "--agent":
			agent, err := adminID(value)
			if err != nil {
				return command, err
			}
			command.agent = agent
		case "--day":
			if _, err := time.Parse(time.DateOnly, value); err != nil {
				return command, errors.New("--day requires YYYY-MM-DD")
			}
			command.day = value
		}
	}
	return command, nil
}

func parsePersonaCreate(command adminCommand, rest []string) (adminCommand, error) {
	if len(rest) != 2 {
		return command, errors.New("persona create expects AGENT and FILE")
	}
	agent, err := adminID(rest[0])
	if err != nil {
		return command, err
	}
	command.agent = agent
	command.path = rest[1]
	return command, nil
}

func parsePersonaSelect(command adminCommand, rest []string) (adminCommand, error) {
	if len(rest) != 2 {
		return command, errors.New("persona select expects AGENT and VERSION")
	}
	agent, err := adminID(rest[0])
	if err != nil {
		return command, err
	}
	version, err := parseAdminInt(rest[1])
	if err != nil || version < 1 {
		return command, errors.New("persona version must be a positive integer")
	}
	command.agent = agent
	command.version = version
	return command, nil
}

func parsePolicySet(command adminCommand, rest []string) (adminCommand, error) {
	if len(rest) != 2 {
		return command, errors.New("policy set expects AGENT and FILE")
	}
	agent, err := adminID(rest[0])
	if err != nil {
		return command, err
	}
	command.agent = agent
	command.path = rest[1]
	return command, nil
}

func parseAgentPauseResume(command adminCommand, rest []string) (adminCommand, error) {
	if len(rest) != 1 {
		return command, errors.New("agent pause/resume expects an AGENT or --all")
	}
	if rest[0] == "--all" {
		command.all = true
		return command, nil
	}
	agent, err := adminID(rest[0])
	if err != nil {
		return command, err
	}
	command.agent = agent
	return command, nil
}

func parseJobList(command adminCommand, rest []string) (adminCommand, error) {
	seen := make(map[string]bool)
	for index := 0; index < len(rest); index++ {
		flag := rest[index]
		switch flag {
		case "--agent", "--status", "--limit", "--cursor":
			if seen[flag] {
				return command, errors.New("duplicate flag")
			}
			seen[flag] = true
			value, err := requireFlagValue(rest, index)
			if err != nil {
				return command, err
			}
			index++
			switch flag {
			case "--agent":
				agent, err := adminID(value)
				if err != nil {
					return command, err
				}
				command.agent = agent
			case "--status":
				switch value {
				case "pending", "running", "retry_wait", "succeeded", "skipped", "cancelled", "failed":
				default:
					return command, errors.New("--status requires an exact status name")
				}
				command.status = value
			case "--limit":
				limit, err := parseAdminInt(value)
				if err != nil || limit < 1 || limit > 64 {
					return command, errors.New("--limit requires 1-64")
				}
				command.limit = limit
			case "--cursor":
				cursor, err := postgres.DecodeAdminCursor(value)
				if err != nil {
					return command, errors.New("cursor token is invalid")
				}
				command.cursor = &cursor
			}
		default:
			return command, errors.New("unexpected flag")
		}
	}
	return command, nil
}

func parseSingleTarget(command adminCommand, rest []string) (adminCommand, error) {
	if len(rest) != 1 {
		return command, fmt.Errorf("%s expects one UUID target", command.name)
	}
	target, err := adminID(rest[0])
	if err != nil {
		return command, err
	}
	command.target = target
	return command, nil
}

func requireFlagValue(rest []string, index int) (string, error) {
	if index+1 >= len(rest) {
		return "", errors.New("missing flag value")
	}
	return rest[index+1], nil
}

// parseAdminInt parses a base-10 integer without accepting prefixes or extra
// characters such as "3junk".
func parseAdminInt(value string) (int, error) {
	if value == "" || value[0] == '+' || value[0] == '-' {
		return 0, errors.New("invalid integer")
	}
	return strconv.Atoi(value)
}

// readAdminPayload returns strictly bounded file bytes; nothing larger than the
// provided cap is ever read, and the contents are never echoed.
func readAdminPayload(path string, max int64) ([]byte, error) {
	if path == "" {
		return nil, errors.New("file path is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("file is not readable")
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, errors.New("file is not readable")
	}
	if int64(len(payload)) > max {
		return nil, errors.New("file is too large")
	}
	return payload, nil
}

func writeAdminJSON(output io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return errors.New("report encoding failed")
	}
	_, writeErr := output.Write(append(encoded, '\n'))
	return writeErr
}

// executeAdminCommand dispatches already-approved trusted operations and read
// models. Errors are safe domain or unavailable classifications from storage.
func executeAdminCommand(ctx context.Context, command adminCommand, store *postgres.Store, output io.Writer) error {
	switch command.name {
	case "persona create":
		payload, err := readAdminPayload(command.path, app.MaxAdminPersonaFileBytes)
		if err != nil {
			return err
		}
		persona, err := app.DecodeAdminPersona(payload, command.agent)
		if err != nil {
			return err
		}
		if err := store.CreateAndSelectPersona(ctx, persona); err != nil {
			return err
		}
		return writeAdminJSON(output, map[string]any{"agent": persona.AgentID, "version": persona.Version, "created": true})
	case "persona select":
		if err := store.SelectAgentPersona(ctx, command.agent, command.version); err != nil {
			return err
		}
		return writeAdminJSON(output, map[string]map[string]any{"selected": {"agent": command.agent, "version": command.version}})
	case "policy set":
		payload, err := readAdminPayload(command.path, app.MaxAdminPolicyFileBytes)
		if err != nil {
			return err
		}
		policy, err := app.DecodeAdminPolicy(payload)
		if err != nil {
			return err
		}
		if err := store.SetAgentPolicy(ctx, command.agent, policy); err != nil {
			return err
		}
		return writeAdminJSON(output, map[string]map[string]any{"policy_set": {"agent": command.agent}})
	case "agent pause", "agent resume":
		enable := command.name == "agent resume"
		if command.all {
			var count int
			var err error
			if enable {
				count, err = store.ResumeAllAgents(ctx)
			} else {
				count, err = store.PauseAllAgents(ctx)
			}
			if err != nil {
				return err
			}
			return writeAdminJSON(output, map[string]any{"action": command.name, "count": count})
		}
		var err error
		if enable {
			err = store.ResumeAgent(ctx, command.agent)
		} else {
			err = store.PauseAgent(ctx, command.agent)
		}
		if err != nil {
			return err
		}
		return writeAdminJSON(output, map[string]any{"action": command.name, "agent": command.agent})
	case "job list":
		return executeAdminJobList(ctx, command, store, output)
	case "job inspect":
		inspection, err := store.InspectGeneration(ctx, command.target)
		if err != nil {
			return err
		}
		return writeAdminJSON(output, map[string]any{"job": inspection.Job, "attempts": inspection.Attempts, "complete": inspection.Complete})
	case "job retry":
		retried, err := store.RetryGeneration(ctx, command.target)
		if err != nil {
			return err
		}
		return writeAdminJSON(output, map[string]any{"job": command.target,
			"status": string(retried.Status), "available_at": retried.AvailableAt.UTC().Format(time.RFC3339Nano)})
	case "usage":
		usage, err := store.DayGenerationUsage(ctx, command.day, command.agent)
		if err != nil {
			return err
		}
		return writeAdminJSON(output, map[string]any{"day": usage.Day, "attempts": usage.Attempts,
			"known_tokens": usage.KnownTokens, "charged_tokens": usage.ChargedTokens, "agents": usage.Agents})
	case "status":
		status, err := store.GenerationStatus(ctx)
		if err != nil {
			return err
		}
		return writeAdminJSON(output, status)
	case "account disable":
		if err := store.DisableAccount(ctx, command.target); err != nil {
			return err
		}
		return writeAdminJSON(output, map[string]any{"agent": command.target, "disabled": true})
	case "post remove":
		if err := store.RemovePost(ctx, command.target); err != nil {
			return err
		}
		return writeAdminJSON(output, map[string]any{"post": command.target, "removed": true})
	case "reply remove":
		if err := store.RemoveReply(ctx, command.target); err != nil {
			return err
		}
		return writeAdminJSON(output, map[string]any{"reply": command.target, "removed": true})
	default:
		return errors.New("unsupported admin command")
	}
}

func executeAdminJobList(ctx context.Context, command adminCommand, store *postgres.Store, output io.Writer) error {
	filter := postgres.AdminJobFilter{AgentID: command.agent, Status: command.status, Cursor: command.cursor, Limit: command.limit}
	jobs, next, err := store.ListGenerationJobs(ctx, filter)
	if err != nil {
		return err
	}
	var cursor string
	if next != nil {
		cursor = *next
	}
	return writeAdminJSON(output, map[string]any{"jobs": jobs, "next_cursor": cursor})
}
