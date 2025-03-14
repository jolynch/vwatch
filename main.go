package main

import (
	"encoding/gob"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jolynch/vwatch/internal"
	"github.com/jolynch/vwatch/internal/api"
	"github.com/jolynch/vwatch/internal/repl"
	"github.com/jolynch/vwatch/internal/util"
	"github.com/zeebo/xxh3"
)

var (
	listen                = "127.0.0.1:8080"
	replicateWith         = ""
	replicateEvery        = 1 * time.Second
	replicateResolveEvery = 10 * time.Second
	fillAddr              = ""
	fillPath              = "/version/${name}"
	fillExpiry            = 10 * time.Second
	fillStrategy          = repl.FillWatch
	blockFor              = 10 * time.Second
	jitterFor             = 1 * time.Second
	logLevel              = new(slog.LevelVar)
	dataLimitBytes        = 4096

	filler    *repl.Filler
	gossiper  *repl.Gossiper
	versions  sync.Map
	watchLock sync.Mutex
	watchers  map[string]Watcher = make(map[string]Watcher)
)


type Watcher struct {
	WatchGroup *sync.WaitGroup
	Signal     chan string
}

func makeWatcher() Watcher {
	var (
		wg sync.WaitGroup
		ch = make(chan string)
	)
	return Watcher{WatchGroup: &wg, Signal: ch}
}

func storeNewVersion(name string, version api.Version) bool {
	versions.Store(name, version)

	watchLock.Lock()
	defer watchLock.Unlock()

	watcher, ok := watchers[name]
	if ok {
		// Safe to delete because we hold the watchLock
		delete(watchers, name)
		// Broadcast the wakeup by closing
		close(watcher.Signal)
		watcher.WatchGroup.Wait()
	}
	return true
}

func putVersion(w http.ResponseWriter, req *http.Request) {
	var (
		name    string = req.PathValue("name")
		version api.Version
		err     error
	)

	if filler != nil {
		http.Error(w, "Replicating nodes cannot accept writes", http.StatusMethodNotAllowed)
		return
	}
	// Name always comes from URL
	version.Name = name

	// Last Modified comes from one of three places
	// 1. Last-Modified header in RFC1123 GMT encoding
	// 2. "modified" URL paremeter in RFC3339 encoding
	// 3. Falls back to the current server time
	m := req.Header.Get(headers.LastModified)
	if m != "" {
		version.LastSync, err = time.Parse(http.TimeFormat, m)
		if err != nil {
			http.Error(w, "Invalid RFC1123 GMT Last-Modified header provided: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		v, ok := req.URL.Query()["modified"]
		if ok && len(v[0]) >= 0 {
			version.LastSync, err = time.Parse(time.RFC3339, v[0])
			if err != nil {
				http.Error(w, "Invalid RFC3339 modified url parameter provided: "+err.Error(), http.StatusBadRequest)
				return
			}
		} else {
			version.LastSync = time.Now()
		}
	}

	// Try to get Version from the version URL parameter
	v, ok := req.URL.Query()["version"]
	if ok && len(v[0]) > 0 {
		version.Version = v[0]
	}

	// Data comes from the first dataLimitBytes of the request body
	buf := make([]byte, dataLimitBytes)
	n, err := io.ReadFull(req.Body, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		http.Error(w, "Error while reading body: "+err.Error(), http.StatusBadRequest)
		return
	}
	version.Data = buf[:n]

	if version.Version == "" {
		checksum := xxh3.Hash128(version.Data)
		version.Version = fmt.Sprintf("xxh3:%08x%08x", checksum.Hi, checksum.Lo)
	}

	upsertVersion(name, version)
	w.Header().Set(headers.ETag, version.Version)
	w.Header().Set(headers.LastModified, version.LastSync.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusNoContent)
}

func getVersion(w http.ResponseWriter, req *http.Request) {
	var (
		timeout time.Duration = blockFor
		err     error
		version api.Version
		name    string = req.PathValue("name")
	)

	t, ok := req.URL.Query()["timeout"]
	if ok && len(t[0]) >= 0 {
		timeout, err = time.ParseDuration(t[0])
		if err != nil {
			http.Error(w, "Invalid timeout duration, try something like 10s", http.StatusBadRequest)
			return
		}
	}
	val, ok := versions.Load(name)
	if !ok {
		if filler != nil {
			params := util.ParseName(name)
			version, err = filler.Fill(params, req.URL.Query())
			if err == nil {
				storeNewVersion(name, version)
				val, ok = versions.Load(name)
			}
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}
	version = val.(api.Version)

	v, ok := req.URL.Query()["version"]
	if ok && len(v[0]) > 0 {
		// Long poll
		if v[0] == version.Version {
			watchLock.Lock()
			watcher, ok := watchers[name]
			if !ok {
				watcher = makeWatcher()
				watchers[name] = watcher
			}
			watcher.WatchGroup.Add(1)
			watchLock.Unlock()

			select {
			case <-watcher.Signal:
				slog.Debug(fmt.Sprintf("Unblocking GET[%s] due to signal", name))
			case <-time.After(timeout):
				slog.Debug(fmt.Sprintf("Unblocking GET[%s] due to %s timeout", name, timeout))
			}
			// The update is holding a lock while waiting for this, so we need to release it asap.
			watcher.WatchGroup.Done()

			val, ok = versions.Load(name)
			if !ok {
				http.Error(w, fmt.Sprintf("%s not found", name), http.StatusNotFound)
			}
			version = val.(api.Version)
			if jitterFor.Milliseconds() > 0 {
				jitterDuration := rand.Int63n(jitterFor.Milliseconds())
				w.Header().Set("Jittered", fmt.Sprintf("%d ms", jitterDuration))
				time.Sleep(time.Duration(jitterDuration) * time.Millisecond)
			}
		}
	}

	w.Header().Set(headers.ETag, version.Version)
	w.Header().Set(headers.LastModified, version.LastSync.UTC().Format(http.TimeFormat))
	w.Header().Set(headers.ContentType, "application/octet-stream")
	w.Write(version.Data)
}

func upsertVersion(name string, version api.Version) bool {
	pv, ok := versions.Load(name)
	if ok {
		prev := pv.(api.Version).LastSync.UnixNano()
		if prev < version.LastSync.UnixNano() && pv.(api.Version).Version != version.Version {
			storeNewVersion(name, version)
			return true
		}
		return false
	} else {
		storeNewVersion(name, version)
		return true
	}
}

func replicate(w http.ResponseWriter, req *http.Request) {
	decoder := gob.NewDecoder(req.Body)
	var remoteVersions []api.Version
	err := decoder.Decode(&remoteVersions)
	if err != nil {
		http.Error(w, "Failed decoding gob data", http.StatusBadRequest)
		return
	}
	var remoteKeys map[string]bool = make(map[string]bool)
	var deltas []api.Version

	for _, version := range remoteVersions {
		remoteKeys[version.Name] = true
		pv, ok := versions.Load(version.Name)
		if ok {
			prev := pv.(api.Version).LastSync.UnixNano()
			if prev > version.LastSync.UnixNano() && pv.(api.Version).Version != version.Version {
				deltas = append(deltas, pv.(api.Version))
			}
		}
	}
	versions.Range(func(key, value any) bool {
		_, seen := remoteKeys[key.(string)]
		if !seen {
			deltas = append(deltas, value.(api.Version))
		}
		return true
	})

	w.Header().Set("Content-Type", "application/octet-stream")
	gob.NewEncoder(w).Encode(deltas)
}

func setLogLevel(w http.ResponseWriter, req *http.Request) {
	level, ok := req.URL.Query()["level"]
	if ok {
		slevel := slog.Level(0)
		err := slevel.UnmarshalText([]byte(level[0]))
		if err != nil {
			return
		}
		slog.SetLogLoggerLevel(slevel)
		io.WriteString(w, logLevel.Level().String())
	} else {
		http.Error(w, "/log requires a ?level=DEBUG param", http.StatusBadRequest)
	}
}

var paths = `Paths
GET /version/{repository}[:{tag}]?[version=last_seen]        -> Get latest version or block for new version
PUT /version/{repository}[:{tag}]?[version=version <- <data> -> Set latest version, unblocking watches
PUT /logging?level=DEBUG                                     -> Set log level

Internal endpoints you should probably avoid unless you know what you are doing
POST /replicate                            <- gob([]Version) -> Replicate state between leaders
`

func main() {
	flag.StringVar(&listen, "listen", listen, "The address to listen on")
	flag.StringVar(&replicateWith, "replicate-with", replicateWith, "Other writeable nodes as a comma separated list. Will resolve DNS and gossip state with all resolved ips")
	flag.DurationVar(&replicateEvery, "replicate-interval", replicateEvery, "How often to exchange state with peers")
	flag.DurationVar(&replicateResolveEvery, "replicate-resolve-interval", replicateResolveEvery, "How often to resolve replicate-with to find peers")

	flag.StringVar(&fillAddr, "fill-addr", fillAddr, "The address to fill from when a version is missing")
	flag.StringVar(&fillPath, "fill-path", fillPath, "The path on the fill host to fill from when a version is missing - can reference {name}, {repository} or {tag}")
	flag.DurationVar(&fillExpiry, "fill-expiry", fillExpiry, "Ask the upstream at least once every interval, for example 10s we would ask upstream every 10s")
	flag.StringVar(&fillStrategy, "fill-strategy", fillStrategy, "Either FILL_WATCH if vwatch upstream, or FILL_CACHE for an upstream that does not support watches")
	flag.IntVar(&dataLimitBytes, "data-limit", dataLimitBytes, "The number of bytes to store from PUTs. Note vwatch is _not_ a database, watch artifacts if you want more than this or store a path to the data")
	flag.DurationVar(&blockFor, "block-for", blockFor, "The duration to block GETs by default for")
	flag.DurationVar(&jitterFor, "jitter-for", jitterFor, "The duration to jitter blocking GETs by, should be less than block-for")
	flag.Parse()

	badStrategy := slices.Contains(repl.ValidFillStrategies, fillStrategy)
	if !badStrategy {
		slog.Error(fmt.Sprintf("Bad -fill-strategy, passed %s but only support %v", fillStrategy, repl.ValidFillStrategies))
		os.Exit(1)
	}

	if fillAddr != "" {
		slog.Info("Creating Filler to replicate from: " + fillAddr)
		client := &http.Client{
			Timeout: blockFor * 2,
		}
		monitor := &sync.Map{}
		filler = &repl.Filler{
			Addr:    fillAddr,
			Path:    fillPath,
			Client:  client,
			Monitor: monitor,
			Channel: make(chan map[string]string, 2),
			FillBody: fillStrategy == repl.FillWatch,
		}
		go filler.Watch(&versions, fillExpiry, storeNewVersion, fillStrategy)
	} else if replicateWith != "" {
		slog.Info("Creating Gossiper to replicate with: " + replicateWith)
		addrs := strings.Split(replicateWith, ",")
		client := &http.Client{
			Timeout: blockFor,
		}
		gossiper = &repl.Gossiper{
			Addrs:      addrs,
			Client:     client,
			LocalState: &versions,
		}
		go gossiper.Gossip(upsertVersion, replicateEvery, replicateResolveEvery)
	}
	http.HandleFunc("PUT /version/{name...}", putVersion)
	http.HandleFunc("GET /version/{name...}", getVersion)
	http.HandleFunc("PUT /logging", setLogLevel)
	http.HandleFunc("POST /replicate", replicate)

	slog.Info(fmt.Sprintf("Listening at %s", listen))
	slog.Info(paths)
	err := http.ListenAndServe(listen, nil)
	if err != nil {
		slog.Error("Failed to bind, is another server listening at this address?")
		os.Exit(1)
	} else {
		slog.Info("All Done!")
	}
}
