package repl

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
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
						Path:   "/v1/versions",
					}

					peers = append(peers, newURL.String())
				}
			} else {
				slog.Warn("Gossiper failed to lookup ips: " + err.Error())
			}
			// Always keep last peer around
			if len(peers) > 0 {
				slices.Sort(peers)
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
	for {
		gossiper.PeerMutex.RLock()
		peersCopy := make([]string, len(gossiper.Peers))
		_ = copy(peersCopy, gossiper.Peers)
		gossiper.PeerMutex.RUnlock()

		// So we exchange state with random nodes, bringing convergence down to worst case 1s * numPeers
		rand.Shuffle(len(peersCopy), func(i, j int) { peersCopy[i], peersCopy[j] = peersCopy[j], peersCopy[i] })
		for _, peer := range peersCopy {
			slog.Debug("Gossip with [" + peer + "]")
			var myVersions []api.Version
			// Never send data to keep Merkle tree as small as possible
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
					if len(theirVersions) < 5 {
						var detail []string
						for _, version := range theirVersions {
							detail = append(detail, version.Format(32))
						}
						slog.Info(fmt.Sprintf(
							"Peer sent %d new versions\n:%s",
							len(theirVersions),
							strings.Join(detail, "\n"),
						))
					} else {
						slog.Info(fmt.Sprintf("Peer sent %d new versions", len(theirVersions)))
					}
				} else {
					slog.Debug(fmt.Sprintf("Peer sent %d versions, none of which were new", len(theirVersions)))
				}
			} else {
				slog.Warn("Gossip failed with : " + err.Error())
			}
			time.Sleep(replicateInterval)
		}
		if len(peersCopy) == 0 {
			time.Sleep(replicateInterval)
		}
	}
}

func (gossiper *Gossiper) Gossip(storeVersion update, replicateInterval, refreshInterval time.Duration) {
	go gossiper.Replicate(storeVersion, replicateInterval)
	for {
		gossiper.findPeers()
		time.Sleep(refreshInterval)
	}
}
