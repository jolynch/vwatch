package api

import (
	"encoding/base64"
	"fmt"
	"time"
	"unicode/utf8"
)

type VersionSet struct {
	Versions []Version `json:"versions"`
}

type Version struct {
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	Data     []byte    `json:"data"`
	LastSync time.Time `json:"last-sync"`
}

func (version Version) SizeBytes() int {
	return len(version.Name) + len(version.Version) + len(version.Data) + 16
}

func (version Version) Format(maxDataLength int) string {
	var data string
	if len(version.Data) < maxDataLength {
		if utf8.Valid(version.Data) {
			data = string(version.Data)
		} else {
			data = base64.StdEncoding.EncodeToString(version.Data)
		}
	} else {
		data = fmt.Sprintf("%d bytes", len(version.Data))
	}
	// JSON doesn't handle binary or timestamps great, just Sprintf it
	return fmt.Sprintf(
		"{name=%s, version=%s, last-sync=%s, data=%s}",
		version.Name, version.Version, version.LastSync.Format(time.RFC3339), data,
	)
}
