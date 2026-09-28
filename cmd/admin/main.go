package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/SophearithSaing/stacktrace-api/internal/app"
	"github.com/SophearithSaing/stacktrace-api/internal/config"
	"github.com/SophearithSaing/stacktrace-api/internal/postgres"
)

// adminCommandDeadline bounds the full command lifetime including the open
// pool; informational reads run on one consistent snapshot each.
const adminCommandDeadline = 30 * time.Second

// maxAdminFileBytes bounds file payloads before they are decoded, so oversized
// persona/policy files never reach the strict-JSON boundary.
const maxAdminFileBytes = app.MaxAdminPersonaFileBytes

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

var adminCommands = map[string]string{
	"persona create": "file", "persona select": "version", "policy set": "file",
	"agent pause": "target", "agent resume": "target",
	"job list": "list", "job inspect": "single", "job retry": "single",
	"usage": "usage", "status": "empty",
	"account disable": "single", "post remove": "single", "reply remove": "single",
}

func adminKind(name string) string {
	if command, ok := adminCommands[name]; ok {
		return command
	}
	return ""
}

// parseAdminCommand is strict: exact command names, each known flag at most
// once, no unknown tokens, mutually exclusive targets, no payload echo and no
// database round trips.
func parseAdminCommand(args []string) (adminCommand, error) {
	var command adminCommand
	if len(args) == 0 {
		return command, errors.New(adminUsage)
	}
	if args[0] == "status" || args[0] == "usage" {
		// Single-word commands: the remainder is flags only.
		kind := adminKind(args[0])
		if kind == "" {
			return command, errors.New(adminUsage)
		}
		command.name = args[0]
		if err := parseAdminFlags(args[1:], &command); err != nil {
			return command, err
		}
		if err := finalizeAdminCommand(&command, kind); err != nil {
			return command, err
		}
		return command, nil
	}
	if len(args) < 2 {
		return command, errors.New(adminUsage)
	}
	kind := adminKind(args[0] + " " + args[1])
	if kind == "" {
		return command, errors.New(adminUsage)
	}
	command.name = args[0] + " " + args[1]
	rest := args[2:]
	if kind != "list" && kind != "usage" {
		// Target commands take exactly their positional slots first.
		switch {
		case kind == "file":
			// AGENT UUID followed by the file path.
			if len(rest) < 2 {
				return command, errors.New("expects AGENT and FILE arguments")
			}
			if !(command.agent == "" && command.path == "") {
				return command, errors.New("one target per command")
			}
			agent, err := adminID(rest[0])
			if err != nil {
				return command, err
			}
			command.agent = agent
			command.path = rest[1]
			rest = rest[2:]
		case kind == "version":
			// AGENT UUID followed by the persona version integer.
			if len(rest) < 2 {
				return command, errors.New("expects AGENT and VERSION arguments")
			}
			agent, err := adminID(rest[0])
			if err != nil {
				return command, err
			}
			if _, err := fmt.Sscanf(rest[1], "%d", &command.version); err != nil || command.version < 1 {
				return command, errors.New("persona version must be a positive integer")
			}
			command.agent = agent
			rest = rest[2:]
		case kind == "target":
			// AGENT UUID or --all (validated in the loop below).
			if len(rest) > 1 {
				return command, errors.New("one target per command")
			}
			if rest[0] == "--all" {
				command.all = true
				rest = rest[1:]
			} else if agent, err := adminID(rest[0]); err != nil {
				return command, err
			} else {
				command.agent = agent
				rest = rest[1:]
			}
		default:
			// One positional UUID target.
			if len(rest) == 0 || len(rest) > 1 {
				return command, errors.New("one target per command")
			}
			target, err := adminID(rest[0])
			if err != nil {
				return command, err
			}
			command.target = target
			rest = rest[1:]
		}
	}
	if err := parseAdminFlags(rest, &command); err != nil {
		return command, err
	}
	if err := finalizeAdminCommand(&command, kind); err != nil {
		return command, err
	}
	return command, nil
}

func parseAdminFlags(rest []string, command *adminCommand) error {
	seen := make(map[string]bool)
	for index := 0; index < len(rest); index++ {
		token := rest[index]
		if !strings.HasPrefix(token, "-") {
			return errors.New("unexpected positional argument after flags")
		}
		value := func() (string, error) {
			if index++; index >= len(rest) {
				return "", errors.New("missing flag value")
			}
			return rest[index], nil
		}
		switch token {
		case "--agent":
			if seen[token] {
				return errors.New("one agent per command")
			}
			agentValue, err := value()
			if err != nil {
				return err
			}
			agent, err := adminID(agentValue)
			if err != nil {
				return err
			}
			seen[token] = true
			command.agent = agent
		case "--status":
			if seen[token] {
				return errors.New("one status per command")
			}
			statusValue, err := value()
			if err != nil {
				return err
			}
			switch statusValue {
			case "pending", "running", "retry_wait", "succeeded", "skipped", "cancelled", "failed":
			default:
				return errors.New("--status requires an exact status name")
			}
			seen[token] = true
			command.status = statusValue
		case "--limit":
			if seen[token] {
				return errors.New("one limit per command")
			}
			limitValue, err := value()
			if err != nil {
				return err
			}
			var limit int
			if _, scanErr := fmt.Sscanf(limitValue, "%d", &limit); scanErr != nil || limit < 1 || limit > 64 {
				return errors.New("--limit requires 1-64")
			}
			seen[token] = true
			command.limit = limit
		case "--cursor":
			if seen[token] {
				return errors.New("one cursor per command")
			}
			cursorValue, err := value()
			if err != nil {
				return err
			}
			cursor, err := postgres.DecodeAdminCursor(cursorValue)
			if err != nil {
				return errors.New("cursor token is invalid")
			}
			seen[token] = true
			command.cursor = &cursor
		case "--day":
			if seen[token] {
				return errors.New("one day per command")
			}
			dayValue, err := value()
			if err != nil {
				return err
			}
			if _, err := time.Parse(time.DateOnly, dayValue); err != nil {
				return errors.New("--day requires YYYY-MM-DD")
			}
			seen[token] = true
			command.day = dayValue
		default:
			return fmt.Errorf("unknown flag %s", token)
		}
	}
	return nil
}

// finalizeAdminCommand guards the exact positional expectations.
func finalizeAdminCommand(command *adminCommand, kind string) error {
	switch kind {
	case "file":
		if command.agent == "" || command.path == "" {
			return errors.New("expects AGENT and FILE arguments")
		}
	case "version":
		if command.agent == "" || command.version < 1 {
			return errors.New("expects AGENT and VERSION arguments")
		}
	case "target":
		if command.agent == "" && !command.all {
			return errors.New("expects an AGENT target or --all")
		}
	case "single":
		if command.target == "" {
			return errors.New("expects a UUID target")
		}
	case "list", "usage", "empty":
	}
	return nil
}

// readAdminPayload returns strictly bounded file bytes; nothing larger than the
// persona cap is ever read, and the contents are never echoed.
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
		payload, err := readAdminPayload(command.path, maxAdminFileBytes)
		if err != nil {
			return err
		}
		persona, err := app.DecodeAdminPersona(payload)
		if err != nil {
			return err
		}
		persona.AgentID = command.agent
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
		payload, err := readAdminPayload(command.path, maxAdminFileBytes)
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
		return writeAdminJSON(output, map[string]any{"job": inspection.Job, "attempts": inspection.Attempts})
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
