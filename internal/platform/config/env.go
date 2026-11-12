// Package config reads service configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// String returns the value of key, or def when it is unset or empty.
func String(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Required returns the value of key, or an error when it is unset or empty.
func Required(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("config: required variable %s is not set", key)
	}
	return v, nil
}

// Int returns key parsed as an int, or def when it is unset or empty.
func Int(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: invalid integer in %s: %w", key, err)
	}
	return n, nil
}

// Float returns key parsed as a float64, or def when it is unset or empty.
func Float(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("config: invalid number in %s: %w", key, err)
	}
	return f, nil
}

// Duration returns key parsed with time.ParseDuration, or def when it is unset or empty.
func Duration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: invalid duration in %s: %w", key, err)
	}
	return d, nil
}
