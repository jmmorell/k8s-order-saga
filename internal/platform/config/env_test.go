package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"k8s-portfolio/internal/platform/config"
)

func TestInt_InvalidValueNamesVariable(t *testing.T) {
	t.Setenv("RATE", "abc")
	_, err := config.Int("RATE", 1)
	require.ErrorContains(t, err, "RATE")
}

func TestRequired_Empty(t *testing.T) {
	_, err := config.Required("MISSING_X")
	require.Error(t, err)
}

func TestString_DefaultAndSet(t *testing.T) {
	require.Equal(t, "d", config.String("UNSET_S", "d"))
	t.Setenv("SET_S", "v")
	require.Equal(t, "v", config.String("SET_S", "d"))
}

func TestRequired_Set(t *testing.T) {
	t.Setenv("REQ", "x")
	v, err := config.Required("REQ")
	require.NoError(t, err)
	require.Equal(t, "x", v)
}

func TestInt_DefaultAndValue(t *testing.T) {
	v, err := config.Int("UNSET_I", 7)
	require.NoError(t, err)
	require.Equal(t, 7, v)
	t.Setenv("I", "42")
	v, err = config.Int("I", 7)
	require.NoError(t, err)
	require.Equal(t, 42, v)
}

func TestFloat(t *testing.T) {
	v, err := config.Float("UNSET_F", 0.5)
	require.NoError(t, err)
	require.InDelta(t, 0.5, v, 1e-9)
	t.Setenv("F", "0.25")
	v, err = config.Float("F", 0.5)
	require.NoError(t, err)
	require.InDelta(t, 0.25, v, 1e-9)
	t.Setenv("F", "x")
	_, err = config.Float("F", 0.5)
	require.ErrorContains(t, err, "F")
}

func TestDuration(t *testing.T) {
	v, err := config.Duration("UNSET_D", time.Second)
	require.NoError(t, err)
	require.Equal(t, time.Second, v)
	t.Setenv("D", "30s")
	v, err = config.Duration("D", time.Second)
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, v)
	t.Setenv("D", "nope")
	_, err = config.Duration("D", time.Second)
	require.ErrorContains(t, err, "D")
}
