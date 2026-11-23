package asset

import (
	"bytes"
	"io"
	"testing"

	"github.com/kdomanski/iso9660"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The read-back goes through github.com/kdomanski/iso9660's reader — an
// independent ISO9660 implementation (test-only dependency; its writer stages
// through temp files, which is why the writer here is hand-rolled).
func TestBuildConfigISO(t *testing.T) {
	config := bytes.Repeat([]byte("version: v1alpha1\nmachine: {}\n"), 300) // ~9 KB, spans sectors

	iso, err := BuildConfigISO(config)
	require.NoError(t, err)
	require.Zero(t, len(iso)%sectorSize, "image must be whole sectors")

	t.Run("primary volume descriptor", func(t *testing.T) {
		pvd := iso[16*sectorSize:]
		assert.Equal(t, byte(1), pvd[0])
		assert.Equal(t, "CD001", string(pvd[1:6]))
		assert.Equal(t, "metal-iso", string(bytes.TrimRight(pvd[40:72], " ")),
			"volume label is what talos.config=metal-iso matches — verbatim, lowercase")
	})

	t.Run("read-back", func(t *testing.T) {
		img, err := iso9660.OpenImage(bytes.NewReader(iso))
		require.NoError(t, err)

		label, err := img.Label()
		require.NoError(t, err)
		assert.Equal(t, "metal-iso", label)

		root, err := img.RootDir()
		require.NoError(t, err)
		children, err := root.GetAllChildren()
		require.NoError(t, err)
		// The reader lists the "." and ".." records too; the kernel hides them.
		var f *iso9660.File
		for _, c := range children {
			if !c.IsDir() {
				require.Nil(t, f, "exactly one file on the medium")
				f = c
			}
		}
		require.NotNil(t, f)
		assert.Equal(t, "config.yaml", f.Name(), "the one file Talos reads")
		assert.Equal(t, int64(len(config)), f.Size())
		got, err := io.ReadAll(f.Reader())
		require.NoError(t, err)
		assert.Equal(t, config, got)
	})

	t.Run("deterministic", func(t *testing.T) {
		again, err := BuildConfigISO(config)
		require.NoError(t, err)
		assert.Equal(t, iso, again, "same config must pack to the same bytes — rebuild guards key on digests")
	})

	t.Run("empty config refused", func(t *testing.T) {
		_, err := BuildConfigISO(nil)
		assert.Error(t, err)
	})
}
