package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGuardTimeout(t *testing.T) {
	assert.Equal(t, 90*time.Second, guardTimeout(90*time.Second))
	// On the command line 0 is "never"; the fleet option reads 0 as the
	// default, so the flag's 0 must become a negative value.
	assert.Negative(t, guardTimeout(0))
	assert.Negative(t, guardTimeout(-time.Second))
}
