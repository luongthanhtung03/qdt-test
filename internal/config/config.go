// Package config loads service configuration from the environment.
//
// Every setting has a usable default except the admin token, which must be set
// explicitly: defaulting an auth credential is how services ship with a known
// password.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable the service reads at boot.
type Config struct {
	Addr           string        // listen address, e.g. ":8080"
	DBPath         string        // path to the SQLite file
	AdminAPIToken  string        // bearer token guarding /api/v1
	PublicBaseURL  string        // absolute base for canonical URLs and sitemap entries
	PollInterval   time.Duration // how often the scheduler looks for due jobs
	LeaseTTL       time.Duration // how long a claimed job stays claimed before it can be stolen
	BatchSize      int           // maximum jobs claimed per poll
	ShutdownGrace  time.Duration // bound on graceful shutdown
	SchedulerOn    bool          // lets tests run the API without a background worker
	LogLevelIsJSON bool          // structured JSON logs instead of human-readable text
}

// Load reads configuration from the process environment.
func Load() (Config, error) {
	c := Config{
		Addr:           envString("ADDR", ":8080"),
		DBPath:         envString("DB_PATH", "./data/qdt.db"),
		AdminAPIToken:  envString("ADMIN_API_TOKEN", ""),
		PublicBaseURL:  strings.TrimRight(envString("PUBLIC_BASE_URL", "http://localhost:8080"), "/"),
		SchedulerOn:    envBool("SCHEDULER_ENABLED", true),
		LogLevelIsJSON: envBool("LOG_JSON", false),
	}

	var err error
	if c.PollInterval, err = envDuration("SCHEDULER_POLL_INTERVAL", time.Second); err != nil {
		return Config{}, err
	}
	if c.LeaseTTL, err = envDuration("SCHEDULER_LEASE_TTL", 30*time.Second); err != nil {
		return Config{}, err
	}
	if c.ShutdownGrace, err = envDuration("SHUTDOWN_TIMEOUT", 10*time.Second); err != nil {
		return Config{}, err
	}
	if c.BatchSize, err = envInt("SCHEDULER_BATCH_SIZE", 10); err != nil {
		return Config{}, err
	}

	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	if c.AdminAPIToken == "" {
		return fmt.Errorf("ADMIN_API_TOKEN must be set (see .env.example)")
	}
	if c.DBPath == "" {
		return fmt.Errorf("DB_PATH must not be empty")
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("SCHEDULER_POLL_INTERVAL must be positive, got %s", c.PollInterval)
	}
	// A lease shorter than the poll interval would let a second worker steal a job
	// while the first is still working on it. Correctness still holds -- the job
	// transaction checks locked_by -- but the wasted work is a config mistake worth
	// refusing rather than absorbing.
	if c.LeaseTTL <= c.PollInterval {
		return fmt.Errorf("SCHEDULER_LEASE_TTL (%s) must exceed SCHEDULER_POLL_INTERVAL (%s)",
			c.LeaseTTL, c.PollInterval)
	}
	if c.BatchSize <= 0 {
		return fmt.Errorf("SCHEDULER_BATCH_SIZE must be positive, got %d", c.BatchSize)
	}
	if !strings.HasPrefix(c.PublicBaseURL, "http://") && !strings.HasPrefix(c.PublicBaseURL, "https://") {
		return fmt.Errorf("PUBLIC_BASE_URL must start with http:// or https://, got %q", c.PublicBaseURL)
	}
	return nil
}

func envString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", key, v)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration (try 1s, 500ms, 2m)", key, v)
	}
	return d, nil
}
