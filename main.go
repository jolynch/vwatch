package main

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
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
	"sync/atomic"
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
	// Two level map of name -> version -> Watcher
	watchers sync.Map
)

type Watcher struct {
	Signal       chan string
	NumConsumers *atomic.Int64
}

func makeWatcher() Watcher {
	var consumer atomic.Int64
	return Watcher{
		Signal:       make(chan string),
		NumConsumers: &consumer,
	}
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

func storeNewVersion(name string, version api.Version) (result api.Version) {
	// Up until now we have done optimistic concurrency control on versions, when we actually go
	// to store the version and wake all watchers on versions not equal to that version, we need
	// to lock for the Compare And Swap operation. Otherwise we could be storing new versions
	// which are then being waited on before we can clean up properly.
	versionLock.Lock()
	defer versionLock.Unlock()

	result = version
	pv, hasVersion := versions.Load(name)

	// Potential optimization if we find this is an issue with artifact cardinality
	// if config.FillStrategy == repl.FillCache && !hasWatch {
	//	return false
	// }

	if hasVersion {
		prev := pv.(api.Version).LastSync.UnixNano()
		if prev < version.LastSync.UnixNano() && pv.(api.Version).Version != version.Version {
			versions.Store(name, version)
		} else {
			version = pv.(api.Version)
			result = version
		}
	} else {
		versions.Store(name, version)
	}

	pw, hasWatch := watchers.Load(name)
	// Only wake watchers if they exist, we may receive writes for names
	// that are not watched at all.
	if hasWatch {
		watchMap := pw.(*sync.Map)
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
	return result
}

func putVersion(w http.ResponseWriter, req *http.Request) {
	var (
		name    string = req.PathValue("name")
		version api.Version
		err     error
	)

	// Do not re-replicate replication requests from the same node
	r, replicate := req.URL.Query()["source"]
	if replicate && len(r[0]) >= 0 {
		if r[0] == config.NodeId {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	if filler != nil && config.FillStrategy == conf.FillWatch {
		http.Error(w, "Replicating nodes in FILL_WATCH cannot accept writes", http.StatusMethodNotAllowed)
		return
	}
	if name == "" {
		http.Error(w, "Must pass a non empty name", http.StatusBadRequest)
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

	// Data comes from the first DataLimitBytes of the request body
	buf := new(bytes.Buffer)
	limit := int64(config.DataLimitBytes)
	n, err := io.CopyN(buf, req.Body, limit+1)
	if err != nil && err != io.EOF {
		http.Error(w, "Error while reading body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if n > limit && config.DataLimitError {
		http.Error(w, fmt.Sprintf("Body larger than DataLimitBytes=%d", limit), http.StatusRequestEntityTooLarge)
		return
	}
	version.Data = buf.Bytes()[:min(limit, n)]

	if version.Version == "" {
		checksum := xxh3.Hash128(version.Data)
		version.Version = fmt.Sprintf("xxh3:%08x%08x", checksum.Hi, checksum.Lo)
	}

	latestVersion := upsertVersion(name, version)
	// ETag must be enclosed in double quotes
	w.Header().Set(headers.ETag, fmt.Sprintf("\"%s\"", latestVersion.Version))
	w.Header().Set(headers.LastModified, latestVersion.LastSync.UTC().Format(http.TimeFormat))

	if latestVersion.Version == version.Version {
		w.WriteHeader(http.StatusNoContent)
		if gossiper != nil && !replicate {
			// Best effort unblock peers
			go gossiper.Write(version)
		}
	} else {
		w.WriteHeader(http.StatusConflict)
	}
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
		if timeout > max(1*time.Minute, config.BlockFor) {
			http.Error(w, fmt.Sprintf("User cannot request block for more than %s", max(1*time.Minute, config.BlockFor)), http.StatusBadRequest)
			return
		}
	}

	val, ok := versions.Load(name)
	if !ok {
		if filler != nil {
			params := parse.ParseName(name)
			resp := make(chan api.Version, 1)
			// Need to respect timeout when making fill calls
			go func(response chan api.Version) {
				version, err := filler.Fill(params, req)
				if err == nil {
					response <- version
				}
				close(response)
			}(resp)

			select {
			case version, ok = <-resp:
				slog.Debug(fmt.Sprintf("Unblocking Fill GET[%s] due to result", name))
				if ok {
					upsertVersion(name, version)
				}
			case <-time.After(timeout):
				slog.Debug(fmt.Sprintf("Unblocking Fill GET[%s] due to %s timeout", name, timeout))
			}
			val, ok = versions.Load(name)
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
		watcher.NumConsumers.Add(1)

		select {
		case <-watcher.Signal:
			slog.Debug(fmt.Sprintf("Unblocking GET[%s] due to signal", name))
		case <-time.After(timeout):
			slog.Debug(fmt.Sprintf("Unblocking GET[%s] due to %s timeout", name, timeout))
		}
		numWatchers := watcher.NumConsumers.Add(-1) + 1

		val, ok = versions.Load(name)
		if !ok {
			http.Error(w, fmt.Sprintf("%s not found", name), http.StatusNotFound)
			return
		}
		version = val.(api.Version)
		if config.JitterPerWatch.Milliseconds() > 0 {
			jitterTarget := numWatchers * config.JitterPerWatch.Milliseconds()
			jitterDuration := min(config.BlockFor, time.Duration(rand.Int63n(jitterTarget))*time.Millisecond)
			timings = append(
				timings,
				fmt.Sprintf("jitter;dur=%s", jitterDuration.Round(time.Millisecond).String()),
			)
			time.Sleep(jitterDuration)
		}
	}

	end := time.Now()
	timings = append(timings, fmt.Sprintf("watch;dur=%s", end.Sub(start).Round(time.Millisecond).String()))

	// ETag must be enclosed in double quotes
	w.Header().Set(headers.ETag, fmt.Sprintf("\"%s\"", version.Version))
	w.Header().Set(headers.LastModified, version.LastSync.UTC().Format(http.TimeFormat))
	w.Header().Set(headers.ContentType, headers.ContentTypeBytes)
	w.Header().Add(headers.ServerTiming, strings.Join(timings, ", "))
	w.WriteHeader(http.StatusOK)
	w.Write(version.Data)
}

func upsertVersion(name string, version api.Version) api.Version {
	pv, ok := versions.Load(name)
	if ok {
		prev := pv.(api.Version).LastSync.UnixNano()
		if prev < version.LastSync.UnixNano() && pv.(api.Version).Version != version.Version {
			return storeNewVersion(name, version)
		}
		return pv.(api.Version)
	} else {
		return storeNewVersion(name, version)
	}
}

func replicate(w http.ResponseWriter, req *http.Request) {
	var (
		versionSet api.VersionSet
		err        error
	)

	contentType := req.Header.Get(headers.ContentType)
	switch contentType {
	case headers.ContentTypeBytes:
		err = gob.NewDecoder(req.Body).Decode(&versionSet)
		if err != nil {
			http.Error(w, "Failed decoding gob data: "+err.Error(), http.StatusBadRequest)
			return
		}
	default:
		contentType = headers.ContentTypeJson
		err = json.NewDecoder(req.Body).Decode(&versionSet)
		if err != nil {
			http.Error(w, "Failed decoding json data: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	slog.Debug(fmt.Sprintf("/v1/versions detected content-type: %s", contentType))

	var (
		remoteVersions []api.Version   = versionSet.Versions
		remoteKeys     map[string]bool = make(map[string]bool)
		deltas         []api.Version   = make([]api.Version, 0)
		// Limit single response to 1MiB at a time (by default)
		budgetBytes = int(config.ReplicateLimitBytes)
	)

	// shuffle remote Versions to ensure progress even if we are hitting our limits
	rand.Shuffle(
		len(remoteVersions),
		func(i, j int) {
			remoteVersions[i], remoteVersions[j] = remoteVersions[j], remoteVersions[i]
		})

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
			slog.Debug(fmt.Sprintf("/v1/versions hitting budget after %d bytes", config.ReplicateLimitBytes))
			return false
		}
		_, seen := remoteKeys[key.(string)]
		if !seen {
			deltas = append(deltas, value.(api.Version))
			budgetBytes -= value.(api.Version).SizeBytes()
		}
		return budgetBytes >= 0
	})

	w.Header().Set(headers.ContentType, contentType)
	w.WriteHeader(http.StatusOK)
	switch contentType {
	case headers.ContentTypeBytes:
		gob.NewEncoder(w).Encode(deltas)
	case headers.ContentTypeJson:
		json.NewEncoder(w).Encode(deltas)
	}
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

var frontendPaths = `Client HTTP Server Paths:
GET /v1/versions/{name}?[version={last_version}]        -> Get latest version or block for new version
`
var backendPaths = `Server HTTP Server Paths:
 PUT /v1/logging?level=DEBUG                            -> Set log level
POST /v1/versions                                       <- gob(VersionSet) | json(VersionSet) -> Replicate state between leaders
 PUT /v1/versions/{name}?[version={version}]            <- {data}                             -> Set latest version, unblocking watches
`

func main() {
	config = conf.FromEnv()

	flag.StringVar(&config.ListenClient, "listen-client", config.ListenClient, "The address to listen on for clients")
	flag.StringVar(&config.ListenServer, "listen-server", config.ListenServer, "The address to listen on for server traffic (replication)")
	flag.StringVar(&config.NodeId, "node-id", config.NodeId, "When replicating identify ourselves with this unique identifier")
	flag.StringVar(&config.ReplicateWith, "replicate-with", config.ReplicateWith, "Other writeable nodes as a comma separated list. Will resolve DNS and gossip state with all resolved ips. This should be their server-listen address!")
	flag.DurationVar(&config.ReplicateEvery, "replicate-interval", config.ReplicateEvery, "How often to exchange state with peers")
	flag.DurationVar(&config.ReplicateResolveEvery, "replicate-resolve-interval", config.ReplicateResolveEvery, "How often to resolve replicate-with to find peers")
	flag.Uint64Var(&config.ReplicateLimitBytes, "replicate-limit-bytes", config.ReplicateLimitBytes, "When bulk replicating, limit responses to this size")
	flag.StringVar(&config.FillFrom, "fill-from", config.FillFrom, "The address to fill from when a version is missing")
	flag.StringVar(&config.FillPath, "fill-path", config.FillPath, "The path on the fill host to fill from when a version is missing - can reference {name}, {repository} or {tag}")
	flag.DurationVar(&config.FillExpiry, "fill-expiry", config.FillExpiry, "Ask the upstream at least once every interval, for example 10s we would ask upstream every 10s")
	flag.StringVar(&config.FillStrategy, "fill-strategy", config.FillStrategy, "Either FILL_WATCH if vwatch upstream, or FILL_CACHE for an upstream that does not support watches")
	flag.Uint64Var(&config.DataLimitBytes, "data-limit-bytes", config.DataLimitBytes, "The number of bytes to store from PUTs. Note vwatch is _not_ a database, watch artifacts if you want more than this or store a path to the data")
	flag.BoolVar(&config.DataLimitError, "data-limit-error", config.DataLimitError, "When data exceeds the limit, should an error occur or just truncate")
	flag.DurationVar(&config.BlockFor, "block-for", config.BlockFor, "The duration to block GETs by default for")
	flag.DurationVar(&config.JitterPerWatch, "jitter-per-watch", config.JitterPerWatch, "The duration to jitter blocking GETs by proportional to the number of outstanding watches on this version")
	flag.Parse()

	badStrategy := slices.Contains(repl.ValidFillStrategies, config.FillStrategy)
	if !badStrategy {
		slog.Error(fmt.Sprintf("Bad -fill-strategy, passed %s but only support %v", config.FillStrategy, repl.ValidFillStrategies))
		os.Exit(2)
	}

	slog.Info("Configuration of Server:\n" + config.PrettyRepr())

	if config.FillFrom != "" {
		setupFill()
	}
	if config.ReplicateWith != "" {
		setupReplication()
	}

	// Setup the read-only client APIs and the writable server APIs
	go listenForClientReads()
	go listenForServerWrites()
	select {}
}

func listenForClientReads() {
	// External "Client" endpoints
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/versions/{name...}", getVersion)
	slog.Info(fmt.Sprintf("Listening for Client Traffic at %s", config.ListenClient))
	slog.Info(frontendPaths)
	err := http.ListenAndServe(config.ListenClient, mux)
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to bind %s, is another server already listening?", config.ListenClient))
		os.Exit(1)
	}
}

func listenForServerWrites() {
	// Internal Writeable endpoints, separated
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/versions", replicate)
	mux.HandleFunc("PUT /v1/versions/{name...}", putVersion)
	mux.HandleFunc("PUT /v1/logging", setLogLevel)
	slog.Info(fmt.Sprintf("Listening for Server traffic at %s", config.ListenServer))
	slog.Info(backendPaths)
	err := http.ListenAndServe(config.ListenServer, mux)
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to bind %s, is another server already listening?", config.ListenServer))
		os.Exit(1)
	}
}

func setupReplication() {
	slog.Info("Creating Gossiper to replicate with: " + config.ReplicateWith)
	addrs := strings.Split(config.ReplicateWith, ",")
	client := &http.Client{
		Timeout: config.BlockFor,
	}
	gossiper = &repl.Gossiper{
		Config:     config,
		Addrs:      addrs,
		Client:     client,
		LocalState: &versions,
	}
	go gossiper.Gossip(upsertVersion, config.ReplicateEvery, config.ReplicateResolveEvery)
}

func setupFill() {
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
		Channel:  make(chan repl.FillRequest, 2),
		FillBody: config.FillStrategy == conf.FillWatch,
	}
	go filler.Watch(&versions, config.FillExpiry, storeNewVersion, config.FillStrategy)
}
