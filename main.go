package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jolynch/vwatch/internal/api"
	"github.com/jolynch/vwatch/internal/repl"
	"github.com/jolynch/vwatch/internal/util"
)

var (
	listen        = "127.0.0.1:8080"
	replicateWith = ""
	fillAddr      = ""
	fillPath      = "/version/${name}"
	blockFor      = 10 * time.Second
	jitterFor     = 1 * time.Second
	logLevel      = new(slog.LevelVar)

	filler    *repl.Filler = nil
	gossiper  *repl.Gossiper = nil
	versions  sync.Map
	watchers  map[string]Watcher = make(map[string]Watcher)
	watchLock sync.Mutex
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

func storeNewVersion(name string, version api.Version) {
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
}

func version(w http.ResponseWriter, req *http.Request) {
	var (
		timeout time.Duration = blockFor
		err     error
		version api.Version
		name    string = req.PathValue("name")
	)

	switch req.Method {
	case "PUT":
		if filler != nil {
			http.Error(w, "Replicating nodes cannot accept writes", http.StatusMethodNotAllowed)
			return
		}
		var lastSync time.Time
		json.NewDecoder(req.Body).Decode(&version)
		version.Name = name
		if version.LastSync == nil {
			lastSync = time.Now()
			version.LastSync = &lastSync
		}
		pv, ok := versions.Load(name)
		if ok {
			prev := pv.(api.Version).LastSync.UnixNano()
			if prev < version.LastSync.UnixNano() && pv.(api.Version).Version != version.Version {
				go storeNewVersion(name, version)
			}
		} else {
			go storeNewVersion(name, version)
		}
	case "GET":
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
				http.Error(w, fmt.Sprintf("{\"error\": \"%s not found\"}", name), http.StatusNotFound)
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
	default:
		http.Error(w, "/version supports only GET and PUT", http.StatusBadRequest)
		return
	}

	w.Header().Set("ETag", version.Version)
	w.Header().Set("Last-Modified", version.LastSync.UTC().Format(http.TimeFormat))
	io.WriteString(w, version.Version)
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
GET /version/{repository}[:{tag}]?[version=last_seen]    -> Get latest version or block for new version
PUT /version/{repository}[:{tag}] {"version": <version>} -> Set latest version, unblocking watches
PUT /log?level=DEBUG                                     -> Set log level
`

func main() {
	flag.StringVar(&listen, "listen", listen, "The address to listen on")
	flag.StringVar(&replicateWith, "replicate-with", replicateWith, "Other writeable nodes as a comma separated list. Will resolve DNS and gossip state with all resolved ips")
	flag.StringVar(&fillAddr, "fill-addr", fillAddr, "The address to fill from when a version is missing")
	flag.StringVar(&fillPath, "fill-path", fillPath, "The path on the fill host to fill from when a version is missing - can reference {name}, {repository} or {tag}")
	flag.DurationVar(&blockFor, "block-for", blockFor, "The duration to block GETs by default for")
	flag.DurationVar(&jitterFor, "jitter-for", jitterFor, "The duration to jitter blocking GETs by, should be less than block-for")
	flag.Parse()

	if fillAddr != "" {
		slog.Info("Creating Filler to replicate from: " + fillAddr)
		client := &http.Client{
			Timeout: blockFor * 2,
		}
		monitor := &sync.Map{}
		filler = &repl.Filler{Addr: fillAddr, Path: fillPath, Client: client, Monitor: monitor, Channel: make(chan string, 2)}
		go filler.Watch(&versions, blockFor, storeNewVersion)
	} else if replicateWith != "" {
		slog.Info("Creating Gossiper to replicate with: " + replicateWith)
		addrs := strings.Split(replicateWith, ",")
		client := &http.Client{
			Timeout: blockFor,
		}
		gossiper = &repl.Gossiper{
			Addrs: addrs,
			Client: client,
			LocalState: &versions,
		}
		go gossiper.Gossip(storeNewVersion)
	}
	http.HandleFunc("/version/{name...}", version)
	http.HandleFunc("PUT /log", setLogLevel)

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
