package api

import (
	"testing"
)

func FuzzTestSizeBytes(f *testing.F) {
	f.Add("repo/image", "v123", []byte("test"))
	f.Add("repo/image:tag", "sha256:abc", []byte("foo"))
	f.Fuzz(func(t *testing.T, name string, version string, data []byte) {
		value := Version{
			Name:    name,
			Version: version,
			Data:    data,
		}
		size := value.SizeBytes()
		// TODO use reflection or something
		expected := len(name) + len(version) + len(data) + 16
		if size != expected {
			t.Errorf("Expected size of %d but found %d", expected, size)
		}
	})

}
