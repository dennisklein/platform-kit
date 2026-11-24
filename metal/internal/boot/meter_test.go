package boot

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func meterFor(image []byte) *meter {
	return newMeter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "node.iso", time.Time{}, bytes.NewReader(image))
	}), int64(len(image)))
}

func get(m *meter, rng string) {
	r := httptest.NewRequest(http.MethodGet, "/node.iso", nil)
	if rng != "" {
		r.Header.Set("Range", rng)
	}
	m.ServeHTTP(httptest.NewRecorder(), r)
}

func closed(m *meter) bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

// The BMC reads the header of a medium when it mounts it: a 64 KiB read
// and some 512-byte reads. With a small config that is the whole image, but
// the machine has not booted yet, so the pull is not complete.
func TestMountProbeIsNotThePull(t *testing.T) {
	m := meterFor(make([]byte, 48<<10))
	get(m, "bytes=0-511")
	get(m, "bytes=0-65535")
	get(m, "")
	assert.False(t, closed(m), "reads before the power action are not the pull")
	assert.Positive(t, m.total(), "ServedBytes still counts them")

	m.arm()
	get(m, "")
	assert.True(t, closed(m), "a whole read after the power action is the pull")
}

// A read repeated after the power action counts once: the pull is complete
// only when every byte was served.
func TestRepeatedRangeCountsOnce(t *testing.T) {
	m := meterFor(make([]byte, 256<<10))
	m.arm()
	for range 8 {
		get(m, "bytes=0-65535")
	}
	assert.False(t, closed(m), "8 x the same 64 KiB is not a 256 KiB image")
	get(m, "bytes=65536-131071")
	get(m, "bytes=196608-")
	assert.False(t, closed(m))
	get(m, "bytes=100000-200000")
	assert.True(t, closed(m), "the ranges cover the image")
}

func TestSpans(t *testing.T) {
	var s spans
	s.add(10, 20)
	s.add(30, 40)
	s.add(15, 35)
	s.add(0, 5)
	assert.Equal(t, spans{{0, 5}, {10, 40}}, s)
	assert.Equal(t, int64(35), s.total())
	s.add(5, 10)
	assert.Equal(t, spans{{0, 40}}, s)
}
