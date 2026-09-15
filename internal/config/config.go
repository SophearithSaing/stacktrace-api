// Package config reads and validates process environment configuration.
package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Role string

const (
	Server   Role = "server"
	Database Role = "database"
)

type Config struct {
	Environment     string
	DatabaseURL     string
	HTTPAddr        string
	APIPublicOrigin string
	ClientOrigins   []string
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
	c := Config{
		Environment: getenv("APP_ENV"),
		DatabaseURL: getenv("DATABASE_URL"),
	}
	if c.Environment == "" {
		c.Environment = "production"
	}
	if c.Environment != "development" && c.Environment != "production" {
		return Config{}, errors.New("APP_ENV must be development or production")
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.Fragment != "" {
		// URL parsing errors can contain credentials; never return them.
		return Config{}, errors.New("DATABASE_URL must be a PostgreSQL connection URL with a host")
	}
	if role == Database {
		return c, nil
	}

	c.HTTPAddr = getenv("HTTP_ADDR")
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":8080"
		if c.Environment == "development" {
			c.HTTPAddr = "127.0.0.1:8080"
		}
	}
	_, port, err := net.SplitHostPort(c.HTTPAddr)
	p, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || p < 1 || p > 65535 {
		return Config{}, errors.New("HTTP_ADDR must be a host:port address with a port from 1 to 65535")
	}
	c.APIPublicOrigin = getenv("API_PUBLIC_ORIGIN")
	if !validOrigin(c.APIPublicOrigin, c.Environment) {
		return Config{}, errors.New("API_PUBLIC_ORIGIN must be an exact HTTPS origin (loopback HTTP is allowed in development)")
	}
	seen := make(map[string]bool)
	for raw := range strings.SplitSeq(getenv("CLIENT_ORIGINS"), ",") {
		origin := strings.TrimSpace(raw)
		if !validOrigin(origin, c.Environment) {
			return Config{}, errors.New("CLIENT_ORIGINS must be a comma-separated list of exact HTTPS origins (loopback HTTP is allowed in development)")
		}
		if !seen[origin] {
			c.ClientOrigins = append(c.ClientOrigins, origin)
			seen[origin] = true
		}
	}
	return c, nil
}

func validOrigin(raw, environment string) bool {
	origin, err := url.Parse(raw)
	if err != nil || origin == nil || origin.Hostname() == "" || origin.User != nil || origin.Opaque != "" ||
		origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" ||
		strings.ContainsAny(raw, "*#\\") || strings.HasSuffix(origin.Host, ":") {
		return false
	}
	if port := origin.Port(); port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
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
