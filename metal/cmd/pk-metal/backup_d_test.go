package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRecipientTails(t *testing.T) {
	assert.Equal(t, "…7k2q9xzw,…aaaabbbb", recipientTails([]string{
		"age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p7k2q9xzw",
		"age1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzaaaabbbb",
	}))
	assert.Equal(t, "short", recipientTails([]string{"short"}), "a value shorter than the tail is shown whole")
	assert.Empty(t, recipientTails(nil))
}
