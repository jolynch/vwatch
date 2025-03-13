package repl

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"time"

	"github.com/jolynch/vwatch/internal/api"
)

type Gossiper struct {
	Addrs      []string
	Client     *http.Client
	LocalState *sync.Map
	PeerMutex  sync.RWMutex
	Peers      []string
}

func (gossiper *Gossiper) findPeers() {
	for _, addr := range gossiper.Addrs {
		var peers []string
		u, err := url.Parse(addr)
		if err == nil {
			host, port, err := net.SplitHostPort(u.Host)
			if err != nil {
				host = u.Host
				port = ""
			}
			ips, err := net.LookupIP(host)
			if err == nil {
				for _, ip := range ips {
					newHost := ip.String()
					if ip.To4() == nil {
						newHost = fmt.Sprintf("[%s]", newHost)
					}
					if port != "" {
						newHost = fmt.Sprintf("%s:%s", newHost, port)
					}
					newURL := url.URL{
						Scheme: u.Scheme,
						Host:   newHost,
						Path:   "/replicate",
					}

					peers = append(peers, newURL.String())
				}
			} else {
				slog.Warn("Gossiper failed to lookup ips: " + err.Error())
			}
			// Always keep last peer around
			if len(peers) > 0 {
				gossiper.PeerMutex.Lock()
				if !reflect.DeepEqual(peers, gossiper.Peers) {
					slog.Info(fmt.Sprintf("Gossiper found %d peers %+v", len(peers), peers))
					gossiper.Peers = peers
				}
				gossiper.PeerMutex.Unlock()
			} else {
				slog.Warn("Gossiper failed to resolve any peers")
			}
		} else {
			slog.Warn(fmt.Sprintf("Gossiper failed to parse address [%s]: %s", addr, err))
		}
	}
}

func (gossiper *Gossiper) Replicate(upsertVersion update, replicateInterval time.Duration) {
	var i int64 = 0
	for {
		gossiper.PeerMutex.RLock()
		if len(gossiper.Peers) > 0 {
			peer := gossiper.Peers[i%int64(len(gossiper.Peers))]
			gossiper.PeerMutex.RUnlock()

			slog.Debug("Gossip with [" + peer + "]")
			var myVersions []api.Version
			gossiper.LocalState.Range(func(key, value any) bool {
				name := value.(api.Version).Name
				version := value.(api.Version).Version
				ts := value.(api.Version).LastSync
				myVersions = append(myVersions, api.Version{Name: name, Version: version, LastSync: ts})
				return true
			})
			var buf bytes.Buffer
			enc := gob.NewEncoder(&buf)
			enc.Encode(myVersions)
			resp, err := gossiper.Client.Post(peer, "application/octet-stream", &buf)
			if err == nil {
				var theirVersions []api.Version
				gob.NewDecoder(resp.Body).Decode(&theirVersions)
				for _, v := range theirVersions {
					upsertVersion(v.Name, v)
				}
				if len(theirVersions) > 0 {
					slog.Info(fmt.Sprintf("Peer sent %d new versions!", len(theirVersions)))
				}
			} else {
				slog.Warn("Gossip failed with : " + err.Error())
			}
		} else {
			gossiper.PeerMutex.RUnlock()
		}

		time.Sleep(replicateInterval)
		i++
	}
}

func (gossiper *Gossiper) Gossip(storeVersion update, replicateInterval, refreshInterval time.Duration) {
	go gossiper.Replicate(storeVersion, replicateInterval)
	for {
		gossiper.findPeers()
		time.Sleep(refreshInterval)
	}
}
