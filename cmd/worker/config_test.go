package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRole(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected Role
		errFunc  require.ErrorAssertionFunc
	}{
		{
			name:     "control",
			input:    "control",
			expected: RoleControl,
		},
		{
			name:     "data",
			input:    "data",
			expected: RoleData,
		},
		{
			name:     "uppercase is normalized",
			input:    "CONTROL",
			expected: RoleControl,
		},
		{
			name:     "surrounding whitespace is trimmed",
			input:    "  data  ",
			expected: RoleData,
		},
		{
			name:    "empty is rejected",
			input:   "",
			errFunc: require.Error,
		},
		{
			name:    "unknown is rejected",
			input:   "worker",
			errFunc: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.errFunc == nil {
				test.errFunc = require.NoError
			}

			role, err := parseRole(test.input)
			test.errFunc(t, err)

			if err != nil {
				assert.Empty(t, role)

				return
			}

			assert.Equal(t, test.expected, role)
		})
	}
}

func TestParseIdleExitAfter(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected time.Duration
		errFunc  require.ErrorAssertionFunc
	}{
		{
			name:     "empty disables idle-exit",
			input:    "",
			expected: 0,
		},
		{
			name:     "whitespace-only disables idle-exit",
			input:    "   ",
			expected: 0,
		},
		{
			name:     "valid duration",
			input:    "15m",
			expected: 15 * time.Minute,
		},
		{
			name:     "surrounding whitespace is trimmed",
			input:    "  30s  ",
			expected: 30 * time.Second,
		},
		{
			name:    "invalid duration is rejected",
			input:   "soon",
			errFunc: require.Error,
		},
		{
			name:    "negative duration is rejected",
			input:   "-5m",
			errFunc: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.errFunc == nil {
				test.errFunc = require.NoError
			}

			idleExitAfter, err := parseIdleExitAfter(test.input)
			test.errFunc(t, err)

			if err != nil {
				assert.Zero(t, idleExitAfter)

				return
			}

			assert.Equal(t, test.expected, idleExitAfter)
		})
	}
}

func TestParseConfig(t *testing.T) {
	t.Run("valid role and log level", func(t *testing.T) {
		t.Setenv("ROLE", "control")
		t.Setenv("LOG_LEVEL", "debug")

		cfg, err := parseConfig()
		require.NoError(t, err)
		assert.Equal(t, RoleControl, cfg.Role)
		assert.Equal(t, "debug", cfg.LogLevel)
	})

	t.Run("idle-exit window is parsed", func(t *testing.T) {
		t.Setenv("ROLE", "control")
		t.Setenv("WORKER_IDLE_EXIT_AFTER", "15m")

		cfg, err := parseConfig()
		require.NoError(t, err)
		assert.Equal(t, 15*time.Minute, cfg.IdleExitAfter)
	})

	t.Run("idle-exit defaults to disabled", func(t *testing.T) {
		t.Setenv("ROLE", "control")
		t.Setenv("WORKER_IDLE_EXIT_AFTER", "")

		cfg, err := parseConfig()
		require.NoError(t, err)
		assert.Zero(t, cfg.IdleExitAfter)
	})

	t.Run("invalid idle-exit window is an error", func(t *testing.T) {
		t.Setenv("ROLE", "control")
		t.Setenv("WORKER_IDLE_EXIT_AFTER", "nope")

		_, err := parseConfig()
		require.Error(t, err)
	})

	t.Run("missing role is an error", func(t *testing.T) {
		t.Setenv("ROLE", "")

		_, err := parseConfig()
		require.Error(t, err)
	})
}

func TestRoleTaskQueue(t *testing.T) {
	assert.Equal(t, controlTaskQueue, RoleControl.taskQueue())
	assert.Equal(t, dataTaskQueue, RoleData.taskQueue())
	// The two roles must never collide on a queue.
	assert.NotEqual(t, RoleControl.taskQueue(), RoleData.taskQueue())
}
