// Package config reads and validates process environment configuration.
package config

import (
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
)

type Role string

const (
	Server   Role = "server"
	Database Role = "database"
)

type Config struct {
	Environment      string
	DatabaseURL      string
	HTTPAddr         string
	APIPublicOrigin  string
	ClientOrigins    []string
	CSRFSigningKey   []byte
	CursorSigningKey []byte
	SecureCookies    bool
}

// Load validates only the settings needed by the requested binary role.
// The production default requires an explicit opt-in to local HTTP settings.
func Load(role Role) (Config, error) {
	return load(role, os.Getenv)
}

func load(role Role, getenv func(string) string) (Config, error) {
	if role != Server && role != Database {
		return Config{}, errors.New("unsupported configuration role")
	}
	cfg := Config{
		Environment: getenv("APP_ENV"),
		DatabaseURL: getenv("DATABASE_URL"),
	}
	if cfg.Environment == "" {
		cfg.Environment = "production"
	}
	if cfg.Environment != "development" && cfg.Environment != "production" {
		return Config{}, errors.New("APP_ENV must be development or production")
	}
	databaseURL, err := url.Parse(cfg.DatabaseURL)
	if err != nil || (databaseURL.Scheme != "postgres" && databaseURL.Scheme != "postgresql") || databaseURL.Hostname() == "" || databaseURL.Fragment != "" {
		// URL parsing errors can contain credentials; never return them.
		return Config{}, errors.New("DATABASE_URL must be a PostgreSQL connection URL with a host")
	}
	if role == Database {
		return cfg, nil
	}

	cfg.HTTPAddr = getenv("HTTP_ADDR")
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
		if cfg.Environment == "development" {
			cfg.HTTPAddr = "127.0.0.1:8080"
		}
	}
	_, port, err := net.SplitHostPort(cfg.HTTPAddr)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return Config{}, errors.New("HTTP_ADDR must be a host:port address with a port from 1 to 65535")
	}
	cfg.APIPublicOrigin = getenv("API_PUBLIC_ORIGIN")
	if !validOrigin(cfg.APIPublicOrigin, cfg.Environment) {
		return Config{}, errors.New("API_PUBLIC_ORIGIN must be an exact HTTPS origin (loopback HTTP is allowed in development)")
	}
	for raw := range strings.SplitSeq(getenv("CLIENT_ORIGINS"), ",") {
		origin := strings.TrimSpace(raw)
		if !validOrigin(origin, cfg.Environment) {
			return Config{}, errors.New("CLIENT_ORIGINS must be a comma-separated list of exact HTTPS origins (loopback HTTP is allowed in development)")
		}
		if !slices.Contains(cfg.ClientOrigins, origin) {
			cfg.ClientOrigins = append(cfg.ClientOrigins, origin)
		}
	}
	cfg.CSRFSigningKey, err = hex.DecodeString(getenv("CSRF_SIGNING_KEY"))
	if err != nil || len(cfg.CSRFSigningKey) != 32 {
		return Config{}, errors.New("CSRF_SIGNING_KEY must encode 32 random bytes as 64 hexadecimal characters")
	}
	cfg.CursorSigningKey, err = hex.DecodeString(getenv("CURSOR_SIGNING_KEY"))
	if err != nil || len(cfg.CursorSigningKey) != 32 {
		return Config{}, errors.New("CURSOR_SIGNING_KEY must encode 32 random bytes as 64 hexadecimal characters")
	}
	if slices.Equal(cfg.CursorSigningKey, cfg.CSRFSigningKey) {
		return Config{}, errors.New("CURSOR_SIGNING_KEY must differ from CSRF_SIGNING_KEY")
	}
	// Non-secure cookies are only possible for validated loopback HTTP origins
	// with explicit development mode. HTTPS always uses the production cookie.
	cfg.SecureCookies = strings.HasPrefix(cfg.APIPublicOrigin, "https://")
	return cfg, nil
}

func validOrigin(raw, environment string) bool {
	origin, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if origin.Hostname() == "" || origin.User != nil || origin.Opaque != "" {
		return false
	}
	if origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
		return false
	}
	if strings.ContainsAny(raw, "*#\\") || strings.HasSuffix(origin.Host, ":") {
		return false
	}
	if port := origin.Port(); port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return false
		}
	}
	if origin.Scheme == "https" {
		return true
	}
	host := origin.Hostname()
	return environment == "development" && origin.Scheme == "http" &&
		(host == "localhost" || net.ParseIP(host).IsLoopback())
}
