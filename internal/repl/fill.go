package repl

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/jolynch/vwatch/internal/api"
	"github.com/jolynch/vwatch/internal/util"
)

const (
	FillWatch = "FILL_WATCH"
	FillCache = "FILL_CACHE"
)

var (
	ValidFillStrategies = []string{FillCache, FillWatch}
)

type Filler struct {
	Addr     string
	Path     string
	FillBody bool
	Client   *http.Client
	Monitor  *sync.Map
	Channel  chan map[string]string
}

func (filler Filler) Fill(nameParams map[string]string, httpParams url.Values) (version api.Version, err error) {
	if filler.Addr == "" {
		err = errors.New("no upstream to fill from")
		return
	}
	name := nameParams["name"]
	urlPath := util.ExpandPattern(filler.Path, nameParams)
	slog.Debug(fmt.Sprintf("Fill [%s] expanded url to: %s", name, urlPath))

	filled, err := url.JoinPath(filler.Addr, urlPath)
	if err != nil {
		slog.Warn(fmt.Sprintf("Fill [%s] url missing http:// in fill-addr: %s", name, err.Error()))
		return
	}
	if len(httpParams) > 0 {
		filled = fmt.Sprintf("%s?%s", filled, httpParams.Encode())
	}
	slog.Info(fmt.Sprintf("Fill [%s] GET %s", name, filled))
	resp, err := filler.Client.Get(filled)
	if err != nil {
		slog.Warn(fmt.Sprintf("Fill [%s] GET call failed: %s", name, err.Error()))
		return
	}

	rv := resp.Header.Get("ETag")
	if rv == "" {
		err = errors.New("no ETag header in response - cannot fill version")
		slog.Warn(fmt.Sprintf("Fill [%s] has %s", name, err.Error()))
		return
	}

	lastSync := time.Now()
	lastModified := resp.Header.Get("Last-Modified")
	if lastModified != "" {
		lastSync, err = time.Parse(http.TimeFormat, lastModified)
		if err != nil {
			slog.Warn(fmt.Sprintf("Fill [%s] invalid sync timestamp: %s", name, err.Error()))
			return
		}
	}

	var data []byte
	if filler.FillBody {
		data, err = io.ReadAll(resp.Body)
		if err != nil {
			slog.Warn(fmt.Sprintf("Fill [%s] error while reading data: %s", name, err.Error()))
			return
		}
	}

	_, ok := filler.Monitor.LoadOrStore(name, nil)
	if !ok {
		slog.Info(fmt.Sprintf("Fill [%s] enqueueing monitor", name))
		filler.Channel <- nameParams
	} else {
		slog.Debug(fmt.Sprintf("Fill [%s] skipping monitor", name))
	}

	return api.Version{
		Name:     name,
		Version:  rv,
		LastSync: lastSync,
		Data:     data,
	}, nil
}

type update func(string, api.Version) bool

func (filler Filler) Watch(state *sync.Map, fillExpiry time.Duration, newVersion update, fillStrategy string) {
	slog.Info("Starting Filler Watch")
	for {
		params := <-filler.Channel
		slog.Info("Spawning Watcher for: [" + params["name"] + "]")
		go watch(filler, params["name"], state, fillExpiry, newVersion, fillStrategy, params)
	}
}

func watch(filler Filler, name string, state *sync.Map, fillExpiry time.Duration, storeNewVersion update, fillStrategy string, nameParams map[string]string) {
	nextVersion := ""
	for {
		params := url.Values{}
		params.Add("timeout", fillExpiry.String())
		params.Add("version", nextVersion)
		pv, prevVersionExists := state.Load(name)
		version, err := filler.Fill(nameParams, params)
		if err != nil {
			if prevVersionExists {
				// Have observed a version in the past, need to keep watching in case it comes back
				backoff := rand.Int63n(fillExpiry.Milliseconds())
				slog.Warn(fmt.Sprintf("Failed while filling [%s], backing off %dms: %s", name, backoff, err.Error()))
				time.Sleep(time.Duration(backoff) * time.Millisecond)
			} else {
				// Have never received a valid version, we cannot watch it yet, let another request attempt to get it
				slog.Warn("Watch for [%s] terminating due to no version")
				filler.Monitor.Delete(name)
				return
			}
		} else {
			nextVersion = version.Version
			if prevVersionExists {
				prevTs := pv.(api.Version).LastSync.UnixNano()
				if prevTs < version.LastSync.UnixNano() && pv.(api.Version).Version != version.Version {
					slog.Info(fmt.Sprintf("Replacing state[%s] with newer version %s compared to %s", name, version, pv.(api.Version)))
					go storeNewVersion(name, version)
				}
			}
			if fillStrategy == FillCache {
				time.Sleep(fillExpiry)
			}
		}
	}
}
