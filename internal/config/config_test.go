package config

import (
	"testing"
	"time"
)

func TestAdvisorDeadline(t *testing.T) {
	t.Setenv("PK_ADVISOR_DEADLINE", "")
	if c := FromEnv(); c.AdvisorDeadline != 12*time.Second {
		t.Errorf("default %v", c.AdvisorDeadline)
	}
	t.Setenv("PK_ADVISOR_DEADLINE", "4s")
	if c := FromEnv(); c.AdvisorDeadline != 4*time.Second {
		t.Errorf("set %v", c.AdvisorDeadline)
	}
	t.Setenv("PK_ADVISOR_DEADLINE", "soon")
	if c := FromEnv(); c.AdvisorDeadline != 12*time.Second {
		t.Errorf("invalid %v", c.AdvisorDeadline)
	}
}
