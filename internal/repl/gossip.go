package repl

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/jolynch/vwatch/internal/api"
)

type Gossiper struct {
	Addrs      []string
	Client     *http.Client
	LocalState *sync.Map
	Peers      []url.URL
}

func (gossiper Gossiper) findPeers() {
	for _, addr := range gossiper.Addrs {
		peers := make([]url.URL, len(gossiper.Addrs))
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

					peers = append(peers, url.URL{
						Scheme: u.Scheme,
						Host:   newHost,
						Path:   "/replicate",
					})
				}
			} else {
				slog.Warn("Gossiper failed to lookup ips: " + err.Error())
			}
			// Always keep last peer around
			if len(peers) > 0 {
				slog.Info(fmt.Sprintf("Found %d peers", len(peers)))
				gossiper.Peers = peers
			} else {
				slog.Warn("Gossiper failed to resolve any peers")
			}
		} else {
			slog.Warn(fmt.Sprintf("Gossiper failed to parse address [%s]: %s", addr, err))
		}
	}
}

func (gossiper Gossiper) Replicate(storeVersion update, replicateInterval time.Duration) {
	var i int64 = 0
	for {
		peers := gossiper.Peers
		peer := peers[i%int64(len(peers))]

		slog.Info("Gossip with :" + peer.Host)
		var myVersions []api.Version
		gossiper.LocalState.Range(func(key, value any) bool {
			name := value.(api.Version).Name
			version := value.(api.Version).Version
			ts := value.(api.Version).LastSync
			myVersions = append(myVersions, api.Version{Name: name, Version: version, LastSync: ts})
			return true
		})
		time.Sleep(replicateInterval)
		i++
	}
}

func (gossiper Gossiper) Gossip(storeVersion update, replicateInterval, refreshInterval time.Duration) {
	go gossiper.Replicate(storeVersion, replicateInterval)
	for {
		gossiper.findPeers()
		slog.Info(fmt.Sprintf("Peers: %+v", gossiper.Peers))
		time.Sleep(refreshInterval)
	}
}
