package redact

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	token    = "0123456789abcdef0123456789abcdef"
	password = "fedcba9876543210fedcba9876543210"
)

func TestString(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{
			name: "Huawei userinfo in the image URL",
			in:   `bmc: VmmControl Connect: 400: {"error":{"message":"Image https://media:` + password + `@10.0.0.1:30443/grants/` + token + `/n1-config.iso is invalid"}}`,
			want: `bmc: VmmControl Connect: 400: {"error":{"message":"Image https://***@10.0.0.1:30443/grants/***/n1-config.iso is invalid"}}`,
		},
		{
			name: "escaped slashes in a JSON body",
			in:   `{"MessageArgs":["https:\/\/media:` + password + `@host\/grants\/` + token + `\/x.iso"]}`,
			want: `{"MessageArgs":["https:\/\/***@host\/grants\/***\/x.iso"]}`,
		},
		{
			name: "grant URL without userinfo",
			in:   "mount https://host:30443/grants/" + token + "/x.iso: status 403",
			want: "mount https://host:30443/grants/***/x.iso: status 403",
		},
		{
			name: "Password action parameter",
			in:   `{"UserName": "media", "Password": "` + password + `", "Image": "x"}`,
			want: `{"UserName": "media", "Password": "***", "Image": "x"}`,
		},
		{
			name: "query of a signed URL",
			in:   "fetch https://s3.example/b/x.iso?X-Amz-Signature=abc&X-Amz-Credential=k failed",
			want: "fetch https://s3.example/b/x.iso?*** failed",
		},
		{
			name: "plain text and a factory URL stay",
			in:   "boot asset installer-1 is not Ready: https://factory.talos.dev/image/abc/v1.14.0/metal-amd64.iso",
			want: "boot asset installer-1 is not Ready: https://factory.talos.dev/image/abc/v1.14.0/metal-amd64.iso",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.in)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, password)
			assert.NotContains(t, got, token)
			assert.Equal(t, got, String(got), "masking twice changes nothing")
		})
	}
}

func TestStringSecretsByValue(t *testing.T) {
	in := `{"MessageArgs":["` + password + `","Password"]}`
	assert.Equal(t, `{"MessageArgs":["***","Password"]}`, String(in, password),
		"an echoed value without its member name is masked when the caller names it")
	assert.Equal(t, "the media user", String("the media user", "media"),
		"a short value is not masked: it is no secret and would mask plain words")
}

func TestError(t *testing.T) {
	assert.NoError(t, Error(nil))

	cause := fmt.Errorf("bmc: InsertMedia: https://media:%s@h/grants/%s/x.iso: %w", password, token, context.DeadlineExceeded)
	err := Error(cause)
	assert.Equal(t, "bmc: InsertMedia: https://***@h/grants/***/x.iso: context deadline exceeded", err.Error())
	assert.ErrorIs(t, err, context.DeadlineExceeded, "the cause stays visible to errors.Is")

	wrapped := fmt.Errorf("step: %w", err)
	assert.NotContains(t, wrapped.Error(), password, "a wrapper prints the masked text")

	outer := Error(fmt.Errorf("again https://u:%s@h/: %w", password, err))
	assert.NotContains(t, outer.Error(), password, "an outer text is masked too")
	assert.True(t, errors.Is(outer, context.DeadlineExceeded))
}
