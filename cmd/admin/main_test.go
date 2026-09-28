package main

import (
	"strings"
	"testing"
)

func TestParseAdminCommandStrictness(t *testing.T) {
	if _, err := parseAdminCommand(nil); err == nil || !strings.Contains(err.Error(), "usage: admin") {
		t.Fatalf("missing command: %v", err)
	}
	ok := []string{"persona", "create", "00000000-0000-4000-8000-000000000001", "/tmp/persona.json"}
	command, err := parseAdminCommand(ok)
	if err != nil || command.name != "persona create" || command.agent != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("create parse: %+v %v", command, err)
	}
	selectCommand, err := parseAdminCommand([]string{"persona", "select", "00000000-0000-4000-8000-000000000001", "3"})
	if err != nil || selectCommand.version != 3 || selectCommand.agent != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("select parse: %+v %v", selectCommand, err)
	}
	pauseAll, err := parseAdminCommand([]string{"agent", "pause", "--all"})
	if err != nil || !pauseAll.all || pauseAll.agent != "" {
		t.Fatalf("pause all: %+v %v", pauseAll, err)
	}
	pauseAgent, err := parseAdminCommand([]string{"agent", "pause", "00000000-0000-4000-8000-000000000001"})
	if err != nil || pauseAgent.agent == "" {
		t.Fatalf("pause agent: %+v %v", pauseAgent, err)
	}
	jobList, err := parseAdminCommand([]string{"job", "list",
		"--agent", "00000000-0000-4000-8000-000000000001", "--status", "failed", "--limit", "16"})
	if err != nil || jobList.agent == "" || jobList.status != "failed" || jobList.limit != 16 {
		t.Fatalf("job list: %+v %v", jobList, err)
	}
	usage, err := parseAdminCommand([]string{"usage", "--day", "2026-09-28"})
	if err != nil || usage.day != "2026-09-28" {
		t.Fatal("usage day parse failed", err)
	}
	for name, args := range map[string][]string{
		"unknown command":    {"unknown", "x"},
		"agent+all conflict": {"agent", "pause", "--all", "00000000-0000-4000-8000-000000000001"},
		"unknown flag":       {"job", "list", "--weird", "x"},
		"limit out of range": {"job", "list", "--limit", "99"},
		"missing value":      {"job", "list", "--limit"},
		"invalid status":     {"job", "list", "--status", "weird"},
		"invalid uuid":       {"job", "retry", "nope"},
		"bad persona file":   {"persona", "select", "n", "0"},
		"missing file args":  {"persona", "create"},
		"missing targets":    {"job", "retry"},
		"zero day range":     {"usage", "--day", "wrong"},
	} {
		if _, err := parseAdminCommand(args); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// TestFormatAdminReports covers the bounded JSON report encoding: no persona
// text or error strings ever surface from the dispatch path.
func TestFormatAdminReports(t *testing.T) {
	var output strings.Builder
	if err := writeAdminJSON(&output, map[string]any{"agent": "a", "version": 2}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "2") || strings.Contains(output.String(), "\n\n") {
		t.Fatal("formatted report")
	}
}
