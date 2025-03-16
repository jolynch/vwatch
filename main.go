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

	"github.com/jolynch/vwatch/internal/api"
	"github.com/jolynch/vwatch/internal/conf"
	"github.com/jolynch/vwatch/internal/headers"
	"github.com/jolynch/vwatch/internal/parse"
	"github.com/jolynch/vwatch/internal/repl"
	"github.com/zeebo/xxh3"
)

var (
	config      conf.Config
	filler      *repl.Filler
	gossiper    *repl.Gossiper
	versionLock sync.Mutex
	versions    sync.Map
	watchers    sync.Map
)

type Watcher struct {
	Signal chan string
}

func makeWatcher() Watcher {
	return Watcher{Signal: make(chan string)}
}

func loadOrStoreWatcher(name, version string) Watcher {
	v, _ := loadOrStoreWatcherMap(name).LoadOrStore(version, makeWatcher())
	return v.(Watcher)
}

func loadOrStoreWatcherMap(name string) *sync.Map {
	versionMap, ok := watchers.Load(name)
	if ok {
		versionMap = versionMap.(*sync.Map)
	} else {
		versionMap = &sync.Map{}
		v, loaded := watchers.LoadOrStore(name, versionMap)
		if loaded {
			versionMap = v.(*sync.Map)
		}
	}
	return versionMap.(*sync.Map)
}

func storeNewVersion(name string, version api.Version) (stored bool) {
	// Up until now we have done optimistic concurrency control on versions, when we actually go
	// to store the version and wake all watchers on versions not equal to that version, we need
	// to lock for the Compare And Swap operation. Otherwise we could be storing new versions
	// which are then being waited on before we can clean up properly.
	versionLock.Lock()
	defer versionLock.Unlock()

	pv, ok := versions.Load(name)
	if ok {
		prev := pv.(api.Version).LastSync.UnixNano()
		if prev < version.LastSync.UnixNano() && pv.(api.Version).Version != version.Version {
			versions.Store(name, version)
		} else {
			version = pv.(api.Version)
			stored = false
		}
	} else {
		versions.Store(name, version)
	}

	// Only wake watchers if they exist, we may receive writes for names
	// that are not watched at all.
	pv, ok = watchers.Load(name)
	if ok {
		watchMap := pv.(*sync.Map)
		latestVersion := version.Version
		// Wake any reads waiting on versions other than this latest version
		watchMap.Range(func(key, value any) bool {
			if latestVersion != key.(string) {
				watcher := value.(Watcher)
				close(watcher.Signal)
				watchMap.Delete(key)
			}
			return true
		})
	}
	return stored
}

func putVersion(w http.ResponseWriter, req *http.Request) {
	var (
		name    string = req.PathValue("name")
		version api.Version
		err     error
	)

	if filler != nil && config.FillStrategy == repl.FillWatch {
		http.Error(w, "Replicating nodes in FILL_WATCH cannot accept writes", http.StatusMethodNotAllowed)
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
	buf := make([]byte, config.DataLimitBytes)
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
	// ETag must be enclosed in double quotes
	w.Header().Set(headers.ETag, fmt.Sprintf("\"%s\"", version.Version))
	w.Header().Set(headers.LastModified, version.LastSync.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusNoContent)
}

func getVersion(w http.ResponseWriter, req *http.Request) {
	var (
		timeout time.Duration = config.BlockFor
		err     error
		version api.Version
		name    string    = req.PathValue("name")
		start   time.Time = time.Now()
		timings []string
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
			params := parse.ParseName(name)
			version, err = filler.Fill(params, req.URL.Query())
			if err == nil {
				upsertVersion(name, version)
				val, ok = versions.Load(name)
			}
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}
	version = val.(api.Version)

	var previousVersion = ""
	etag := req.Header.Get(headers.ETag)
	if etag != "" {
		// Take the version from the header if present
		previousVersion = parse.ParseETagToVersion(etag)
	} else {
		// Otherwise take the version from the URL parameter
		v, ok := req.URL.Query()["version"]
		if ok && len(v[0]) > 0 {
			previousVersion = parse.ParseETagToVersion(v[0])

		}
	}
	if previousVersion != "" && previousVersion == version.Version {
		// Long poll on this version
		watcher := loadOrStoreWatcher(name, version.Version)

		select {
		case <-watcher.Signal:
			slog.Debug(fmt.Sprintf("Unblocking GET[%s] due to signal", name))
		case <-time.After(timeout):
			slog.Debug(fmt.Sprintf("Unblocking GET[%s] due to %s timeout", name, timeout))
		}

		val, ok = versions.Load(name)
		if !ok {
			http.Error(w, fmt.Sprintf("%s not found", name), http.StatusNotFound)
		}
		version = val.(api.Version)
		if config.JitterFor.Milliseconds() > 0 {
			jitterDuration := time.Duration(rand.Int63n(config.JitterFor.Milliseconds())) * time.Millisecond
			timings = append(timings, fmt.Sprintf("jitter;dur=%s", jitterDuration.Round(time.Millisecond).String()))
			time.Sleep(jitterDuration)
		}
	}

	end := time.Now()
	timings = append(timings, fmt.Sprintf("watch;dur=%s", end.Sub(start).Round(time.Millisecond).String()))

	// ETag must be enclosed in double quotes
	w.Header().Set(headers.ETag, fmt.Sprintf("\"%s\"", version.Version))
	w.Header().Set(headers.LastModified, version.LastSync.UTC().Format(http.TimeFormat))
	w.Header().Set(headers.ContentType, "application/octet-stream")
	w.Header().Add(headers.ServerTiming, strings.Join(timings, ", "))
	w.WriteHeader(http.StatusOK)
	w.Write(version.Data)
}

func upsertVersion(name string, version api.Version) bool {
	pv, ok := versions.Load(name)
	if ok {
		prev := pv.(api.Version).LastSync.UnixNano()
		if prev < version.LastSync.UnixNano() && pv.(api.Version).Version != version.Version {
			return storeNewVersion(name, version)
		}
		return false
	} else {
		return storeNewVersion(name, version)
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
	// Limit single response to 1MiB at a time (by default)
	var budgetBytes = int(config.ReplicateLimitBytes)

	for _, version := range remoteVersions {
		remoteKeys[version.Name] = true
		pv, ok := versions.Load(version.Name)
		if ok {
			prevVersion := pv.(api.Version)
			prev := prevVersion.LastSync.UnixNano()
			if prev > version.LastSync.UnixNano() && prevVersion.Version != version.Version {
				deltas = append(deltas, prevVersion)
				budgetBytes -= prevVersion.SizeBytes()
			}
		}
		if budgetBytes < 0 {
			break
		}
	}
	versions.Range(func(key, value any) bool {
		if budgetBytes < 0 {
			return false
		}
		_, seen := remoteKeys[key.(string)]
		if !seen {
			deltas = append(deltas, value.(api.Version))
			budgetBytes -= value.(api.Version).SizeBytes()
		}
		return budgetBytes >= 0
	})

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	gob.NewEncoder(w).Encode(deltas)
}

func setLogLevel(w http.ResponseWriter, req *http.Request) {
	level, ok := req.URL.Query()["level"]
	if ok && len(level[0]) > 0 {
		logLevel, err := conf.LogLevel(level[0], slog.LevelInfo)
		if err != nil {
			http.Error(w, fmt.Sprintf("Invalid log level [%s]: %s", level[0], err.Error()), http.StatusBadRequest)
			return
		}
		slog.SetLogLoggerLevel(logLevel)
		io.WriteString(w, logLevel.Level().String())
		return
	}
	http.Error(w, "/log requires a ?level=DEBUG param", http.StatusBadRequest)
}

var paths = `HTTP Server Paths:

GET /version/{repository}[:{tag}]?[version=last_seen]         -> Get latest version or block for new version
PUT /version/{repository}[:{tag}]?[version=version] <- <data> -> Set latest version, unblocking watches
PUT /logging?level=DEBUG                                      -> Set log level

Internal endpoints you should probably avoid unless you know what you are doing
POST /replicate                            <- gob([]Version) -> Replicate state between leaders
`

func main() {
	config = conf.FromEnv()

	flag.StringVar(&config.Listen, "listen", config.Listen, "The address to listen on")
	flag.StringVar(&config.ReplicateWith, "replicate-with", config.ReplicateWith, "Other writeable nodes as a comma separated list. Will resolve DNS and gossip state with all resolved ips")
	flag.DurationVar(&config.ReplicateEvery, "replicate-interval", config.ReplicateEvery, "How often to exchange state with peers")
	flag.DurationVar(&config.ReplicateResolveEvery, "replicate-resolve-interval", config.ReplicateResolveEvery, "How often to resolve replicate-with to find peers")

	flag.StringVar(&config.FillFrom, "fill-from", config.FillFrom, "The address to fill from when a version is missing")
	flag.StringVar(&config.FillPath, "fill-path", config.FillPath, "The path on the fill host to fill from when a version is missing - can reference {name}, {repository} or {tag}")
	flag.DurationVar(&config.FillExpiry, "fill-expiry", config.FillExpiry, "Ask the upstream at least once every interval, for example 10s we would ask upstream every 10s")
	flag.StringVar(&config.FillStrategy, "fill-strategy", config.FillStrategy, "Either FILL_WATCH if vwatch upstream, or FILL_CACHE for an upstream that does not support watches")
	flag.Uint64Var(&config.DataLimitBytes, "data-limit", config.DataLimitBytes, "The number of bytes to store from PUTs. Note vwatch is _not_ a database, watch artifacts if you want more than this or store a path to the data")
	flag.DurationVar(&config.BlockFor, "block-for", config.BlockFor, "The duration to block GETs by default for")
	flag.DurationVar(&config.JitterFor, "jitter-for", config.JitterFor, "The duration to jitter blocking GETs by, should be less than block-for")
	flag.Parse()

	badStrategy := slices.Contains(repl.ValidFillStrategies, config.FillStrategy)
	if !badStrategy {
		slog.Error(fmt.Sprintf("Bad -fill-strategy, passed %s but only support %v", config.FillStrategy, repl.ValidFillStrategies))
		os.Exit(2)
	}

	slog.Info("Configuration of Server:\n" + config.PrettyRepr())

	if config.FillFrom != "" {
		slog.Info("Creating Filler to replicate from: " + config.FillFrom)
		client := &http.Client{
			Timeout: config.BlockFor * 2,
		}
		monitor := &sync.Map{}
		filler = &repl.Filler{
			Addr:     config.FillFrom,
			Path:     config.FillPath,
			Client:   client,
			Monitor:  monitor,
			Channel:  make(chan map[string]string, 2),
			FillBody: config.FillStrategy == repl.FillWatch,
		}
		go filler.Watch(&versions, config.FillExpiry, storeNewVersion, config.FillStrategy)
	}
	if config.ReplicateWith != "" {
		slog.Info("Creating Gossiper to replicate with: " + config.ReplicateWith)
		addrs := strings.Split(config.ReplicateWith, ",")
		client := &http.Client{
			Timeout: config.BlockFor,
		}
		gossiper = &repl.Gossiper{
			Addrs:      addrs,
			Client:     client,
			LocalState: &versions,
		}
		go gossiper.Gossip(upsertVersion, config.ReplicateEvery, config.ReplicateResolveEvery)
	}
	http.HandleFunc("PUT /version/{name...}", putVersion)
	http.HandleFunc("GET /version/{name...}", getVersion)
	http.HandleFunc("PUT /logging", setLogLevel)
	http.HandleFunc("POST /replicate", replicate)

	slog.Info(fmt.Sprintf("Listening at %s", config.Listen))
	slog.Info(paths)
	err := http.ListenAndServe(config.Listen, nil)
	if err != nil {
		slog.Error("Failed to bind, is another server listening at this address?")
		os.Exit(1)
	} else {
		slog.Info("All Done!")
	}
}
