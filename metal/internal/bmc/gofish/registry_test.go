package gofish

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The four captured spellings per box (2026-07-21): what the Bios resource
// advertises vs the Registries member id vs the member's Registry field. Both
// vendors disagree with themselves — this test pins the normalisation against
// the recorded reality.
func TestRegistryMatches(t *testing.T) {
	t.Run("x12", func(t *testing.T) {
		// Bios: BiosAttributeRegistry.v1_0_0 · member id bare · Registry 1.0.0-dotted
		assert.True(t, registryMatches("BiosAttributeRegistry.v1_0_0", "BiosAttributeRegistry", "BiosAttributeRegistry.1.0.0"))
	})
	t.Run("ibmc", func(t *testing.T) {
		// Bios: 7.6.2 · member id v7_6_2 · Registry truncated to 7.6
		assert.True(t, registryMatches("BiosAttributeRegistry.7.6.2", "BiosAttributeRegistry.v7_6_2", "BiosAttributeRegistry.7.6"))
	})
	t.Run("no-false-positives", func(t *testing.T) {
		assert.False(t, registryMatches("BiosAttributeRegistry.v1_0_0", "Base", "Base.1.0.0"),
			"message registries must not match")
		assert.False(t, registryMatches("BiosAttributeRegistry", "BiosAttributeRegistry2", ""),
			"prefix matching stops at dot boundaries")
		assert.False(t, registryMatches("", "BiosAttributeRegistry", ""),
			"an empty advertisement matches nothing")
	})
}
