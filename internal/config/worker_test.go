package config

import (
	"strings"
	"testing"
)

func TestExecuteRoleExclusiveProviderCredentials(t *testing.T) {
	for _, role := range []Role{Server, Database, WorkerExecute} {
		env := serverEnvironment()
		env["TOGETHER_API_KEY"] = "private-provider-key"
		reads := 0
		cfg, err := load(role, func(key string) string {
			if key == "TOGETHER_API_KEY" {
				reads++
				if role != WorkerExecute {
					t.Fatal("non-execute role read key")
				}
			}
			return env[key]
		})
		if err != nil {
			t.Fatal(err)
		}
		if role == WorkerExecute {
			if reads != 1 || cfg.TogetherAPIKey != env["TOGETHER_API_KEY"] || cfg.HTTPAddr != "" {
				t.Fatal("wrong execute settings")
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
