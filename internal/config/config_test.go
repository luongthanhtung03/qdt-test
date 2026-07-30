package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/luongthanhtung03/qdt-test/internal/config"
)

// setEnv applies a set of environment variables for one test, restoring the
// previous state afterwards via t.Setenv.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	// Clear everything the loader reads, so a variable set in the developer's
	// shell cannot change the result of a test.
	for _, k := range []string{
		"ADDR", "DB_PATH", "ADMIN_API_TOKEN", "PUBLIC_BASE_URL",
		"SCHEDULER_POLL_INTERVAL", "SCHEDULER_LEASE_TTL", "SCHEDULER_BATCH_SIZE",
		"SHUTDOWN_TIMEOUT", "SCHEDULER_ENABLED", "LOG_JSON",
	} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// TestLoad_RequiresAdminToken is the important one: a service that defaults its
// auth credential ships with a known password.
func TestLoad_RequiresAdminToken(t *testing.T) {
	setEnv(t, nil)

	_, err := config.Load()
	require.Error(t, err, "config must not load without an admin token")
	require.Contains(t, err.Error(), "ADMIN_API_TOKEN")
}

func TestLoad_Defaults(t *testing.T) {
	setEnv(t, map[string]string{"ADMIN_API_TOKEN": "secret"})

	cfg, err := config.Load()
	require.NoError(t, err)

	require.Equal(t, ":8080", cfg.Addr)
	require.Equal(t, "./data/qdt.db", cfg.DBPath)
	require.Equal(t, time.Second, cfg.PollInterval)
	require.Equal(t, 30*time.Second, cfg.LeaseTTL)
	require.Equal(t, 10, cfg.BatchSize)
	require.True(t, cfg.SchedulerOn)
}

func TestLoad_Overrides(t *testing.T) {
	setEnv(t, map[string]string{
		"ADMIN_API_TOKEN":         "secret",
		"ADDR":                    ":9999",
		"DB_PATH":                 "/tmp/other.db",
		"PUBLIC_BASE_URL":         "https://cms.example.com/",
		"SCHEDULER_POLL_INTERVAL": "250ms",
		"SCHEDULER_LEASE_TTL":     "5s",
		"SCHEDULER_BATCH_SIZE":    "3",
		"SCHEDULER_ENABLED":       "false",
	})

	cfg, err := config.Load()
	require.NoError(t, err)

	require.Equal(t, ":9999", cfg.Addr)
	require.Equal(t, 250*time.Millisecond, cfg.PollInterval)
	require.Equal(t, 5*time.Second, cfg.LeaseTTL)
	require.Equal(t, 3, cfg.BatchSize)
	require.False(t, cfg.SchedulerOn)
	require.Equal(t, "https://cms.example.com", cfg.PublicBaseURL,
		"the trailing slash must be trimmed so joined URLs do not double up")
}

// TestLoad_Rejects covers the misconfigurations worth failing fast on.
func TestLoad_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			// A lease shorter than the poll interval lets a second worker
			// steal a job that is still being worked on. Correctness holds,
			// but the wasted work means someone typed the wrong value.
			name: "lease shorter than poll interval",
			env:  map[string]string{"SCHEDULER_POLL_INTERVAL": "10s", "SCHEDULER_LEASE_TTL": "1s"},
			want: "SCHEDULER_LEASE_TTL",
		},
		{
			name: "unparseable duration",
			env:  map[string]string{"SCHEDULER_POLL_INTERVAL": "soon"},
			want: "SCHEDULER_POLL_INTERVAL",
		},
		{
			name: "non-numeric batch size",
			env:  map[string]string{"SCHEDULER_BATCH_SIZE": "many"},
			want: "SCHEDULER_BATCH_SIZE",
		},
		{
			name: "zero batch size",
			env:  map[string]string{"SCHEDULER_BATCH_SIZE": "0"},
			want: "SCHEDULER_BATCH_SIZE",
		},
		{
			name: "base URL without a scheme",
			env:  map[string]string{"PUBLIC_BASE_URL": "cms.example.com"},
			want: "PUBLIC_BASE_URL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"ADMIN_API_TOKEN": "secret"}
			for k, v := range tc.env {
				env[k] = v
			}
			setEnv(t, env)

			_, err := config.Load()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}
