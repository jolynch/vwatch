package repl

import (
	"net/http"
	"sync"
	"time"
)

type Gossiper struct {
	Addrs []string
	Client *http.Client
	LocalState *sync.Map
}

func (gossiper Gossiper) Gossip(storeVersion update) {
	for {
		time.Sleep(100 * time.Millisecond)
	}
}