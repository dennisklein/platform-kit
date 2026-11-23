package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoggableBlanksSecrets(t *testing.T) {
	// The bodies a BMC sees that carry a secret: a session login, the media
	// credentials of InsertMedia, and an image URL with userinfo (the iBMC's
	// only credential mechanism).
	assert.Equal(t, `{"UserName":"admin","Password":"***"}`,
		loggable([]byte(`{"UserName":"admin","Password":"s3cr\"et"}`)))
	assert.Equal(t, `{"Image":"https://assets/node.iso", "UserName":"media", "Password": "***"}`,
		loggable([]byte("{\"Image\":\"https://assets/node.iso\",\n  \"UserName\":\"media\",\n  \"Password\": \"grant\"}")))
	assert.Equal(t, `{"VmmControlType":"Connect","Image":"https://***@assets/node.iso"}`,
		loggable([]byte(`{"VmmControlType":"Connect","Image":"https://media:grant@assets/node.iso"}`)))

	long := loggable([]byte(`{"Attributes":{"X":"` + strings.Repeat("a", 2000) + `"}}`))
	assert.LessOrEqual(t, len(long), 520, "a long body is cut")
}
