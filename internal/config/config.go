// Package config reads the settings of all pk commands from the environment.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the environment of a pk process. Each command uses the parts it needs.
type Config struct {
	// Addr is the listen address of pk bench (PK_ADDR, default 127.0.0.1:8080).
	Addr string
	// DataDir holds worktrees, clones, lab results and logs (PK_DATA, default ~/.local/share/pk).
	DataDir string
	// Demo enables auto-login and the demo endpoints (PK_DEMO=1).
	Demo bool
	// User is the person the bench runs for (PK_USER, default dennis).
	User string
	// AdminToken logs a browser in with /?token= and authorizes the CLI as User
	// (PK_ADMIN_TOKEN; generated into <data>/admin-token when empty).
	AdminToken string
	// AllowedHosts are extra Host/Origin values the bench accepts (PK_ALLOWED_HOSTS, comma separated).
	AllowedHosts []string
	// PublicURL is the URL people open (PK_PUBLIC_URL, default http://127.0.0.1:<port>).
	PublicURL string

	// ForgeURL is the forge base URL (PK_FORGE_URL, default http://localhost:3000).
	ForgeURL string
	// ForgeCredentials is the JSON file that pk forge-init writes (PK_FORGE_CREDENTIALS,
	// default <data>/forge.json).
	ForgeCredentials string
	// ForgeOwner and ForgeRepo name the state repo (PK_FORGE_OWNER=platform, PK_FORGE_REPO=state).
	ForgeOwner string
	ForgeRepo  string

	// FluxURL is the fluxsim base URL (PK_FLUX_URL, default http://127.0.0.1:8081).
	FluxURL string
	// FluxAddr is the listen address of pk fluxsim (PK_FLUX_ADDR, default 127.0.0.1:8081).
	FluxAddr string

	// LLM settings. Empty BaseURL means mock mode.
	LLMBaseURL    string // PK_LLM_BASE_URL
	LLMModel      string // PK_LLM_MODEL
	LLMAPIKey     string // PK_LLM_API_KEY
	LLMEmbedModel string // PK_LLM_EMBED_MODEL
	// LLMTimeout bounds one model call (PK_LLM_TIMEOUT, default 45s).
	LLMTimeout time.Duration
	// AdvisorDeadline bounds the model phrasing of an advisor recipe
	// (PK_ADVISOR_DEADLINE, default 12s). After it the advisor answers with
	// its deterministic points.
	AdvisorDeadline time.Duration

	// LabSpeed scales lab step durations (PK_LAB_SPEED, default 1 makes a run last about 25 s;
	// 10 makes it 10 times faster).
	LabSpeed float64

	// PollInterval is how often pk ci and fluxsim poll (PK_POLL_INTERVAL, default 3s).
	PollInterval time.Duration
}

// FromEnv reads the configuration from the environment.
func FromEnv() Config {
	c := Config{
		Addr:             env("PK_ADDR", "127.0.0.1:8080"),
		DataDir:          env("PK_DATA", defaultDataDir()),
		Demo:             envBool("PK_DEMO"),
		User:             env("PK_USER", "dennis"),
		AdminToken:       os.Getenv("PK_ADMIN_TOKEN"),
		AllowedHosts:     split(os.Getenv("PK_ALLOWED_HOSTS")),
		PublicURL:        os.Getenv("PK_PUBLIC_URL"),
		ForgeURL:         strings.TrimRight(env("PK_FORGE_URL", "http://localhost:3000"), "/"),
		ForgeCredentials: os.Getenv("PK_FORGE_CREDENTIALS"),
		ForgeOwner:       env("PK_FORGE_OWNER", "platform"),
		ForgeRepo:        env("PK_FORGE_REPO", "state"),
		FluxURL:          strings.TrimRight(env("PK_FLUX_URL", "http://127.0.0.1:8081"), "/"),
		FluxAddr:         env("PK_FLUX_ADDR", "127.0.0.1:8081"),
		LLMBaseURL:       strings.TrimRight(os.Getenv("PK_LLM_BASE_URL"), "/"),
		LLMModel:         os.Getenv("PK_LLM_MODEL"),
		LLMAPIKey:        os.Getenv("PK_LLM_API_KEY"),
		LLMEmbedModel:    os.Getenv("PK_LLM_EMBED_MODEL"),
		LLMTimeout:       envDuration("PK_LLM_TIMEOUT", 45*time.Second),
		AdvisorDeadline:  envDuration("PK_ADVISOR_DEADLINE", 12*time.Second),
		LabSpeed:         envFloat("PK_LAB_SPEED", 1),
		PollInterval:     envDuration("PK_POLL_INTERVAL", 3*time.Second),
	}
	if c.ForgeCredentials == "" {
		c.ForgeCredentials = filepath.Join(c.DataDir, "forge.json")
	}
	if c.PublicURL == "" {
		c.PublicURL = "http://" + publicHost(c.Addr)
	}
	return c
}

// Port returns the port of Addr.
func (c Config) Port() string {
	if i := strings.LastIndex(c.Addr, ":"); i >= 0 {
		return c.Addr[i+1:]
	}
	return "8080"
}

// Hosts returns the Host header values the bench accepts.
func (c Config) Hosts() []string {
	p := c.Port()
	hosts := []string{"127.0.0.1:" + p, "localhost:" + p, "[::1]:" + p}
	return append(hosts, c.AllowedHosts...)
}

// Paths inside DataDir.
func (c Config) RepoDir() string      { return filepath.Join(c.DataDir, "repo") }
func (c Config) MainDir() string      { return filepath.Join(c.DataDir, "main") }
func (c Config) WorktreesDir() string { return filepath.Join(c.DataDir, "worktrees") }
func (c Config) LabsDir() string      { return filepath.Join(c.DataDir, "labs") }
func (c Config) LLMDir() string       { return filepath.Join(c.DataDir, "llm") }
func (c Config) StateDir() string     { return filepath.Join(c.DataDir, "state") }

func publicHost(addr string) string {
	if strings.HasPrefix(addr, "0.0.0.0:") || strings.HasPrefix(addr, ":") {
		return "127.0.0.1:" + addr[strings.LastIndex(addr, ":")+1:]
	}
	return addr
}

func defaultDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "pk")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".pk-data"
	}
	return filepath.Join(home, ".local", "share", "pk")
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string) bool {
	b, _ := strconv.ParseBool(os.Getenv(k))
	return b
}

func envFloat(k string, def float64) float64 {
	if f, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil && f > 0 {
		return f
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(k)); err == nil && d > 0 {
		return d
	}
	return def
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
