package gofish

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzOemActions: the OEM action table is read from untrusted BMC JSON. It
// must not panic, must give the same answer every time, and must take each
// target from the document, never compose one.
func FuzzOemActions(f *testing.F) {
	for _, seed := range []string{
		`{"Actions":{"Oem":{"#VirtualMedia.VmmControl":{"target":"/redfish/v1/Managers/1/VirtualMedia/CD/Oem/Huawei/Actions/VirtualMedia.VmmControl"}}}}`,
		`{"Actions":{"Oem":{"Huawei":{"#VirtualMedia.VmmControl":{"target":"/a"}}}}}`,
		`{"Oem":{"Huawei":{"Actions":{"#VirtualMedia.VmmControl":{"target":"/b"}}}}}`,
		`{"Actions":{"Oem":null},"Oem":null}`,
		`[]`,
		``,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		first := oemActions(raw)
		for range 20 {
			require.Equal(t, first, oemActions(raw), "oemActions is not deterministic")
		}
		if len(first) == 0 {
			return
		}
		var doc any
		require.NoError(t, json.Unmarshal(raw, &doc))
		strs := map[string]bool{}
		collectStrings(doc, strs)
		for name, target := range first {
			assert.True(t, strs[target], "target %q of %q is not a string in the document", target, name)
			assert.True(t, strs[name] || keyIn(doc, name), "action %q is not named in the document", name)
		}
	})
}

// collectStrings gathers every string value in a decoded JSON document.
func collectStrings(v any, into map[string]bool) {
	switch v := v.(type) {
	case string:
		into[v] = true
	case []any:
		for _, e := range v {
			collectStrings(e, into)
		}
	case map[string]any:
		for _, e := range v {
			collectStrings(e, into)
		}
	}
}

// keyIn reports whether key names an object member anywhere in v.
func keyIn(v any, key string) bool {
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			if keyIn(e, key) {
				return true
			}
		}
	case map[string]any:
		for k, e := range v {
			if k == key || keyIn(e, key) {
				return true
			}
		}
	}
	return false
}

// FuzzOemVendor: the vendor read from an Oem block is one of its keys, and
// the same one on every read.
func FuzzOemVendor(f *testing.F) {
	for _, seed := range []string{`{"Huawei":{}}`, `{}`, `null`, `[]`, ``} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		first := oemVendor(raw)
		for range 20 {
			require.Equal(t, first, oemVendor(raw), "oemVendor is not deterministic")
		}
		if first != "" {
			var m map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &m))
			assert.Contains(t, m, first)
		}
	})
}

// FuzzStripGzipSuffix: the tag is changed only when mod_deflate's suffix is
// there, and then only that suffix is cut. A non-empty tag stays non-empty.
func FuzzStripGzipSuffix(f *testing.F) {
	for _, seed := range []string{`"abc-gzip"`, `W/"abc-gzip"`, `"abc"`, `-gzip"`, ``, `"a-gzip-gzip"`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, etag string) {
		out := stripGzipSuffix(etag)
		if !strings.HasSuffix(etag, `-gzip"`) {
			assert.Equal(t, etag, out)
			return
		}
		assert.Equal(t, etag, strings.TrimSuffix(out, `"`)+`-gzip"`, "more than the suffix was cut")
		assert.NotEmpty(t, out)
	})
}
