package api

import "time"

type Version struct {
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	Data     []byte    `json:"data"`
	LastSync time.Time `json:"last-sync"`
}
