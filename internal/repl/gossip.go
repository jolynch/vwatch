package repl

import (
	"bytes"
	"context"
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
	"github.com/jolynch/vwatch/internal/conf"
	"github.com/jolynch/vwatch/internal/headers"
)

type Gossiper struct {
	Config     conf.Config
	Addrs      []string
	Client     *http.Client
	LocalState *sync.Map
	PeerMutex  sync.RWMutex
	Peers      []string
}

func (gossiper *Gossiper) findPeers() {
	var peers []string
	for _, addr := range gossiper.Addrs {
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
		} else {
			slog.Warn(fmt.Sprintf("Gossiper failed to parse address [%s]: %s", addr, err))
		}
	}
	// Always keep last peer around until we have more
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
}

type WriteResult struct {
	Successful []string
	Failed     []string
}

// Best effort write to peers. Note that we send to everyone async and
// wait for either success or failure with a 1s deadline attached to each
// write to guarantee progress. This method may block for up to that timeout
// and does not guarantee that all nodes have received the new version.
func (gossiper *Gossiper) Write(version api.Version) (result WriteResult, err error) {
	if version.Name == "" || version.Version == "" {
		return
	}
	start := time.Now()

	gossiper.PeerMutex.RLock()
	peersCopy := make([]string, len(gossiper.Peers))
	_ = copy(peersCopy, gossiper.Peers)
	gossiper.PeerMutex.RUnlock()

	var (
		req     *http.Request
		wg      sync.WaitGroup
		success chan string = make(chan string, len(peersCopy))
		failed  chan string = make(chan string, len(peersCopy))
	)

	for _, peer := range peersCopy {
		uri := peer + "/" + url.PathEscape(version.Name)
		if version.Version != "" {
			params := url.Values{}
			params.Add("version", version.Version)
			params.Add("modified", fmt.Sprintf("%d", version.LastSync.UnixMicro()))
			params.Add("source", gossiper.Config.NodeId)
			uri = fmt.Sprintf("%s?%s", uri, params.Encode())
		}

		wg.Add(1)
		go func(peer string, uri string, version api.Version, success chan string, failure chan string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()

			req, err = http.NewRequestWithContext(ctx, http.MethodPut, uri, bytes.NewBuffer(version.Data))
			if err != nil {
				failure <- peer
				return
			}
			req.Header.Set(headers.ContentType, headers.ContentTypeBytes)

			_, err = gossiper.Client.Do(req)
			if err == nil {
				success <- peer
			} else {
				failure <- peer
			}
		}(peer, uri, version, success, failed)
	}
	wg.Wait()
	// Since all results are back we can close
	for _, _ = range peersCopy {
		select {
		case s := <-success:
			result.Successful = append(result.Successful, s)
		case f := <-failed:
			result.Failed = append(result.Failed, f)
		}
	}
	close(success)
	close(failed)

	delta := time.Since(start)
	if len(peersCopy) > 0 {
		slog.Info(fmt.Sprintf("Sent [%s] to %d/%d peers after %s", version.Format(0), len(result.Successful), len(peersCopy), delta))
		if len(result.Failed) > 0 {
			slog.Warn(fmt.Sprintf("Failed to replicate [%s] to: %v", version.Name, result.Failed))
		}
	}
	return result, err
}

// Periodic version set exchange between peers. Runs in a loop, constantly sending
// our hash tree and asking for any deltas.
func (gossiper *Gossiper) Replicate(upsertVersion update, replicateInterval time.Duration) {
	for {
		gossiper.PeerMutex.RLock()
		peersCopy := make([]string, len(gossiper.Peers))
		_ = copy(peersCopy, gossiper.Peers)
		gossiper.PeerMutex.RUnlock()

		// So we exchange state with random nodes, bounding convergence to worst case 1s * numPeers
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
			versionSet := api.VersionSet{
				Versions: myVersions,
			}

			enc := gob.NewEncoder(&buf)
			enc.Encode(versionSet)
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
