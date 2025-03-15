package api

import "time"

type Version struct {
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	Data     []byte    `json:"data"`
	LastSync time.Time `json:"last-sync"`
}

func (version Version) SizeBytes() int {
	return len(version.Name) + len(version.Version) + len(version.Data) + 16
}
