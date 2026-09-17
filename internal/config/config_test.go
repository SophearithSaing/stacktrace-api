package config

import (
	"strings"
	"testing"
)

func serverEnvironment() map[string]string {
	return map[string]string{
		"APP_ENV":           "development",
		"DATABASE_URL":      "postgres://stacktrace:private-password@localhost:5432/stacktrace?sslmode=disable",
		"API_PUBLIC_ORIGIN": "http://localhost:8080",
		"CLIENT_ORIGINS":    "http://localhost:5173",
		"CSRF_SIGNING_KEY":  strings.Repeat("ab", 32),
	}
}

func TestLoadRoles(t *testing.T) {
	env := serverEnvironment()
	getenv := func(key string) string { return env[key] }
	cfg, err := load(Server, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != "127.0.0.1:8080" || cfg.APIPublicOrigin != env["API_PUBLIC_ORIGIN"] || len(cfg.ClientOrigins) != 1 || cfg.ClientOrigins[0] != env["CLIENT_ORIGINS"] {
		t.Fatalf("unexpected local server settings: address=%q origin=%q clients=%v", cfg.HTTPAddr, cfg.APIPublicOrigin, cfg.ClientOrigins)
	}
	if cfg.SecureCookies || len(cfg.CSRFSigningKey) != 32 {
		t.Fatal("incorrect development cookie/key settings")
	}
	delete(env, "API_PUBLIC_ORIGIN")
	delete(env, "CLIENT_ORIGINS")
	delete(env, "CSRF_SIGNING_KEY")
	if _, err := load(Database, getenv); err != nil {
		t.Fatalf("database role should not require HTTP settings: %v", err)
	}
	if _, err := load(Server, getenv); err == nil {
		t.Fatal("server accepted missing origins")
	}
	if _, err := load("worker", getenv); err == nil {
		t.Fatal("accepted an unimplemented role")
	}
}

func TestLoadProductionDefaults(t *testing.T) {
	env := serverEnvironment()
	delete(env, "APP_ENV")
	getenv := func(key string) string { return env[key] }
	if _, err := load(Server, getenv); err == nil {
		t.Fatal("implicit production mode accepted HTTP origins")
	}
	env["API_PUBLIC_ORIGIN"] = "https://api.example.com"
	env["CLIENT_ORIGINS"] = "https://app.example.com, https://app.example.com,https://client.example.com"
	cfg, err := load(Server, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Environment != "production" || cfg.HTTPAddr != ":8080" || len(cfg.ClientOrigins) != 2 {
		t.Fatal("incorrect production defaults or origin deduplication")
	}
	if !cfg.SecureCookies {
		t.Fatal("production cookies must be secure")
	}
}

func TestLoadRejectsInvalidSettings(t *testing.T) {
	tests := []struct{ key, value string }{
		{"APP_ENV", "staging"},
		{"CSRF_SIGNING_KEY", ""},
		{"CSRF_SIGNING_KEY", strings.Repeat("g", 64)},
		{"CSRF_SIGNING_KEY", strings.Repeat("ab", 31)},
		{"DATABASE_URL", ""},
		{"DATABASE_URL", "mysql://localhost/stacktrace"},
		{"DATABASE_URL", "postgres://private-password%zz@localhost/db"},
		{"HTTP_ADDR", "localhost"},
		{"HTTP_ADDR", "localhost:0"},
		{"HTTP_ADDR", "localhost:65536"},
		{"API_PUBLIC_ORIGIN", ""},
		{"API_PUBLIC_ORIGIN", "https://api.example.com/path"},
		{"CLIENT_ORIGINS", ""},
		{"CLIENT_ORIGINS", "http://localhost:5173,"},
		{"CLIENT_ORIGINS", "*"},
		{"CLIENT_ORIGINS", "null"},
		{"CLIENT_ORIGINS", "http://external.example.com"},
		{"CLIENT_ORIGINS", "https://app.example.com/"},
		{"CLIENT_ORIGINS", "https://*.example.com"},
		{"CLIENT_ORIGINS", "https://private-password@app.example.com"},
		{"CLIENT_ORIGINS", "https://app.example.com?"},
		{"CLIENT_ORIGINS", "https://app.example.com#"},
		{"CLIENT_ORIGINS", "https://app.example.com:0"},
		{"CLIENT_ORIGINS", "https://app.example.com:65536"},
		{"CLIENT_ORIGINS", "https://app.example.com:"},
	}
	for i, tt := range tests {
		t.Run(tt.key+"/"+strings.ReplaceAll(tt.value, "/", "_"), func(t *testing.T) {
			env := serverEnvironment()
			env[tt.key] = tt.value
			_, err := load(Server, func(key string) string { return env[key] })
			if err == nil {
				t.Fatalf("case %d accepted invalid %s", i, tt.key)
			}
			if strings.Contains(err.Error(), "private-password") {
				t.Fatal("configuration error disclosed credentials")
			}
		})
	}
}

func TestDevelopmentLoopbackOrigins(t *testing.T) {
	for _, origin := range []string{"http://localhost:5173", "http://127.0.0.1:5173", "http://[::1]:5173", "https://app.example.com"} {
		if !validOrigin(origin, "development") {
			t.Errorf("rejected valid development origin %q", origin)
		}
	}
}
