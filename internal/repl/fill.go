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
	"github.com/jolynch/vwatch/internal/conf"
	"github.com/jolynch/vwatch/internal/parse"
)

var (
	ValidFillStrategies = []string{conf.FillCache, conf.FillWatch}
)

type FillRequest struct {
	NameParams map[string]string
	URL        string
}

type Filler struct {
	Addr         string
	Path         string
	FillBody     bool
	FillExpiry   time.Duration
	FillStrategy string
	FillHeaders  http.Header
	Client       *http.Client
	Monitor      *sync.Map
	Channel      chan FillRequest
}

func (filler *Filler) Fill(nameParams map[string]string, req *http.Request) (version api.Version, err error) {
	if filler.Addr == "" {
		err = errors.New("no upstream to fill from")
		return
	}
	var (
		httpParams = req.URL.Query()
		name       = nameParams["name"]
		filledURL  string
	)

	urlPath, err := parse.ExpandPattern(filler.Path, nameParams)
	if err != nil {
		return
	}
	slog.Debug(fmt.Sprintf("Fill [%s] expanded url to: %s", name, urlPath))

	fillerURL, err := url.JoinPath(filler.Addr, urlPath)
	if err != nil {
		slog.Warn(fmt.Sprintf("Fill [%s] url missing http:// in fill-addr: %s", name, err.Error()))
		return
	}
	if len(httpParams) > 0 {
		filledURL = fmt.Sprintf("%s?%s", fillerURL, httpParams.Encode())
	} else {
		filledURL = fillerURL
	}
	slog.Info(fmt.Sprintf("Fill [%s] GET %s", name, filledURL))

	// Inject any headers we have been asked to
	newReq, _ := http.NewRequest("GET", filledURL, nil)
	for hdr, values := range filler.FillHeaders {
		for _, value := range values {
			newReq.Header.Add(hdr, value)
		}
	}

	resp, err := filler.Client.Do(newReq)
	if err != nil {
		slog.Warn(fmt.Sprintf("Fill [%s] GET call failed: %s", name, err.Error()))
		return
	}

	remoteVersion := resp.Header.Get("ETag")
	if remoteVersion == "" {
		err = errors.New("no ETag header in response - cannot fill version")
		slog.Warn(fmt.Sprintf("Fill [%s] has %s", name, err.Error()))
		return
	}
	remoteVersion = parse.ParseETagToVersion(remoteVersion)

	lastSync := time.Now()
	lastModified := resp.Header.Get("Last-Modified")
	if lastModified != "" {
		lastSync, err = time.Parse(http.TimeFormat, lastModified)
		if err != nil {
			slog.Warn(fmt.Sprintf("Fill [%s] invalid sync timestamp: %s", name, err.Error()))
			return
		}
	}

	data := []byte{}
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
		filler.Channel <- FillRequest{NameParams: nameParams, URL: fillerURL}
	} else {
		slog.Debug(fmt.Sprintf("Fill [%s] skipping monitor", name))
	}

	return api.Version{
		Name:     name,
		Version:  remoteVersion,
		LastSync: lastSync,
		Data:     data,
	}, nil
}

type update func(string, api.Version) api.Version

func (filler *Filler) Watch(state *sync.Map, newVersion update) {
	slog.Info("Starting Filler Watch")
	for {
		fillRequest := <-filler.Channel
		params := fillRequest.NameParams
		slog.Info(fmt.Sprintf("Spawning Watcher for: [%s]", params["name"]))
		go watch(filler, params["name"], state, newVersion, fillRequest)
	}
}

func watch(filler *Filler, name string, state *sync.Map, storeNewVersion update, fillRequest FillRequest) {
	nameParams := fillRequest.NameParams
	nextVersion := ""
	fillExpiry := filler.FillExpiry

	for {
		params := url.Values{}
		params.Add("timeout", fillExpiry.String())
		params.Add("version", nextVersion)
		fillReq, _ := http.NewRequest("GET", fmt.Sprintf("%s?%s", fillRequest.URL, params.Encode()), nil)

		pv, prevVersionExists := state.Load(name)
		version, err := filler.Fill(nameParams, fillReq)
		if err != nil {
			if prevVersionExists {
				// Have observed a version in the past, need to keep watching in case it comes back
				backoff := fillExpiry.Milliseconds() + rand.Int63n(fillExpiry.Milliseconds())
				slog.Warn(fmt.Sprintf("Failed while filling [%s], waiting %dms: %s", name, backoff, err.Error()))
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
					slog.Info(
						fmt.Sprintf(
							"Replacing state[%s] with newer version %s compared to %s",
							name, version.Format(32), pv.(api.Version).Format(32),
						))
					go storeNewVersion(name, version)
				}
			}
			if filler.FillStrategy == conf.FillCache {
				time.Sleep(fillExpiry)
			}
		}
	}
}
