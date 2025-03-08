package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

var (
	listen    = "127.0.0.1:8080"
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

type Version struct {
	Name     string     `json:"name"`
	Version  string     `json:"version"`
	LastSync *time.Time `json:"last-sync"`
}

func storeNewVersion(name string, version Version) {
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
		timeout time.Duration = time.Duration(10 * time.Second)
		err     error
		version Version
		name    string = req.PathValue("name")
	)

	switch req.Method {
	case "PUT":
		var lastSync time.Time
		json.NewDecoder(req.Body).Decode(&version)
		version.Name = name
		if version.LastSync == nil {
			lastSync = time.Now()
			version.LastSync = &lastSync
		}
		pv, ok := versions.Load(name)
		if ok {
			prev := pv.(Version).LastSync.UnixNano()
			if prev < version.LastSync.UnixNano() && pv.(Version).Version != version.Version {
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
				http.Error(w, "Invalid timeout, try something like 10s", http.StatusBadRequest)
				return
			}
		}
		val, ok := versions.Load(name)
		if !ok {
			http.Error(w, fmt.Sprintf("{\"error\": \"%s not found\"}", name), http.StatusNotFound)
			return
		}
		version = val.(Version)

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
					log.Printf("Unblocking due to signal")
				case <-time.After(timeout):
					log.Printf("Unblocking due to %s timeout", timeout)
				}
				watcher.WatchGroup.Done()

				val, ok = versions.Load(name)
				if !ok {
					http.Error(w, fmt.Sprintf("%s not found", name), http.StatusNotFound)
				}
				version = val.(Version)
			}
		}
	default:
		http.Error(w, "/version supports only GET and PUT", http.StatusBadRequest)
		return
	}

	w.Header().Set("Version", version.Version)
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(version)
	if err != nil {
		log.Printf("ERROR %s", err)
	}
}

func main() {
	flag.StringVar(&listen, "listen", listen, "The address to listen on, defaults to 127.0.0.1:8080")
	flag.Parse()

	http.HandleFunc("/version/{name}", version)

	log.Printf("Listening at %s/version", listen)
	err := http.ListenAndServe(listen, nil)
	if err != nil {
		log.Fatalf("Failed to bind, is another server listening at this address?")
	} else {
		log.Printf("Success!")
	}
}
