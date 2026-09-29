package config

import (
	"maps"
	"strings"
	"testing"
)

func TestWorkerServeRejectsInvalidListener(t *testing.T) {
	base := map[string]string{
		"APP_ENV":          "development",
		"DATABASE_URL":     "postgres://localhost/test",
		"TOGETHER_API_KEY": "provider-key",
	}
	for _, addr := range []string{"localhost", "localhost:0", "localhost:65536"} {
		env := maps.Clone(base)
		env["WORKER_HTTP_ADDR"] = addr
		_, err := load(WorkerServe, func(key string) string { return env[key] })
		if err == nil {
			t.Fatalf("accepted invalid WORKER_HTTP_ADDR %q", addr)
		}
	}
}

func TestExecuteRoleExclusiveProviderCredentials(t *testing.T) {
	for _, role := range []Role{Server, Database, WorkerExecute, WorkerServe} {
		env := serverEnvironment()
		env["TOGETHER_API_KEY"] = "private-provider-key"
		reads := 0
		cfg, err := load(role, func(key string) string {
			if key == "TOGETHER_API_KEY" {
				reads++
				if role != WorkerExecute && role != WorkerServe {
					t.Fatal("non-worker role read key")
				}
			}
			return env[key]
		})
		if err != nil {
			t.Fatal(err)
		}
		if role == WorkerExecute || role == WorkerServe {
			if reads != 1 || cfg.TogetherAPIKey != env["TOGETHER_API_KEY"] {
				t.Fatal("wrong worker settings")
			}
		} else if cfg.TogetherAPIKey != "" || reads != 0 {
			t.Fatal("provider key crossed roles")
		}
	}
	for _, key := range []string{"", "private-provider-key"} {
		cfg, err := load(WorkerExecute, func(name string) string {
			switch name {
			case "APP_ENV":
				return "development"
			case "DATABASE_URL":
				return "postgres://localhost/test"
			case "TOGETHER_API_KEY":
				return key
			default:
				t.Fatalf("execute read unrelated setting %s", name)
				return ""
			}
		})
		if key == "" {
			if err == nil || !strings.Contains(err.Error(), "TOGETHER_API_KEY") {
				t.Fatal(err)
			}
		} else if err != nil || cfg.TogetherAPIKey != key {
			t.Fatal(err)
		}
	}
}
