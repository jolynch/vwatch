package repl

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/jolynch/vwatch/internal/api"
	"github.com/jolynch/vwatch/internal/util"
)

type Filler struct {
	Addr string
	Path string
	Client *http.Client
}

func (filler Filler) Fill(nameParams map[string]string, httpParams url.Values) (version api.Version, err error) {
	if filler.Addr == "" {
		err = errors.New("no upstream to fill from")
		return
	}
	urlPath := util.ExpandPattern(filler.Path, nameParams)
	slog.Debug(fmt.Sprintf("Expanded url to %s", urlPath))
	
	filled, err := url.JoinPath(filler.Addr, urlPath)
	if err != nil {
		return
	}
	if len(httpParams) > 0 {
		filled = fmt.Sprintf("%s?%s", filled, httpParams.Encode())
	}
	slog.Info(fmt.Sprintf("GET %s", filled))
	resp, err := filler.Client.Get(filled)
	if err != nil {
		return
	}
	name := nameParams["name"]

	rv := resp.Header.Get("ETag")
	if rv == "" {
		rv = resp.Header.Get("Docker-Content-Digest")
	}
	if rv == "" {
		err = errors.New("no ETag header in response")
		return
	}

	lastSync := resp.Header.Get("Last-Modified")
	if lastSync == "" {
		err = errors.New("no Last-Modified header in response")
		return
	}

	lastSyncTs, err := time.Parse(http.TimeFormat, lastSync)
	if err != nil {
		return
	}

	return api.Version{
		Name: name,
		Version: rv,
		LastSync: &lastSyncTs, 
	}, nil
}

func (filler Filler) Watch(state *sync.Map) {
	for {
		// call Fill with blocking calls and goroutines
	}
}