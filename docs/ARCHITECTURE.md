# Architecture
The goal of `vwatch` is efficient and rapid notification to a large number of
watchers, possibly millions, that a new `version` of a `name` is available.
It is architected to handle many watches on a single machine, but it
scales better both for writes (notification) and reads (watch/wake) with a
leader-follower architecture.

Nodes can either `FILL` from an upstream `vwatch` (`FILL_VWATCH`), in which
case they are `followers`, or can `FILL` from an upstream source (`FILL_CACHE`).
In the later mode, nodes accept `PUT` operations and replicate from their
upstream source of truth. While you can run without a source of truth,
`vwatch` makes no effort to be a durable database nor provide a guaranteed
ordering of writes - you should probably have an upstream like a
`docker registry` or `S3` or some other `HTTP` service which returns
[`ETag` headers](https://en.wikipedia.org/wiki/HTTP_ETag) on resources.

* `Leader` (`L`): Resolves the provided `replicate-with` DNS names and makes a
  best effort to replicate and gossip version sets with these peers.
* `Follower` (`F`): Fills from an upstream `vwatch` leader, cannot accept
  writes but will notify downstreams on a version notification from the
  leaders.
* `Watchers` (`W`): Runs on your autoscaling fleet of nodes, watching followers
  for changes those nodes are interested in.

```txt
  [upstream source]                            [Source of Truth]
      |       \
  [L_1] <-> [L_2] .. [L_i]                     [i Leaders]
      |  \  /  \
  [F_1] [F_2] [F_3] ... [F_k]                  [k Followers]
    |     \   /  \
  [W_1]  [W_2] ... [W_3] ... [W_n]             [n Watchers]
```
Connections from watchers to followers and from followers to leaders are
short-lived, lasting at most the configured watch duration, by default `10s`.
This is so we naturally rebalance watch load continuously.

These layers dramatically reduce load on the upstream source of truth, for
example imagine an alternative polling based solution where every watcher
polled the source of truth every 30s. Without `vwatch`, a quarter million
watchers (`250k`) would generate nearly `10,000 RPS` to the upstream
and have a `delay` of up to `30s`. **With `vwatch`** that upstream load is
reduced to `0.06 RPS` and if writes notify the leaders, `~instant delay`.

## Replication
`Leaders` continuously resolve the `replicate-with` DNS names to discover
peers, as well as replicating peers through gossip messages.

When `Leaders` accept `PUT` operations at `/v1/versions/{name}`, they accept
the write if the `version` that is conveyed is both different and later
(per `last-sync`). If accepted, a best effort write to other leaders is sent.

Every leader periodically (`1s`) gossips their full `VersionSet` sans `Data` to
a peer, and that peer responds with any `Version`s that are either newer
or missing. As the peers list are shuffled and paired with the best effort async
writes, this converges notifications in best case `O(network delay)`,
typical case `O(log(#L)) * 1s`, and worst case `O(#L) * 1s`.

There is no guarantee that `Followers` see notifications in order, strong
read-after-write is not a property of this protocol.

## Filling
`Leaders` periodically poll (default `10s`)  their upstream source of truth
for any changes they may have missed via `PUT` operations. Even if you never
write to `Leaders`, they will eventually converge to the upstream source of
truth.

`Followers` set watches via `GET /v1/versions/{name}` on every `name` they
have been asked for, and fill state from their upstream. These `GET`s unblock
when changes are made, or the timeout is reached.

# Protocol
There are two main protocols involved in `vwatch`, one for notifying watchers,
and the other for watching for new versions.

## Datastructure
The core datastructure of `vwatch` is a `Version`:
```golang
type Version struct {
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	Data     []byte    `json:"data"`
	LastSync time.Time `json:"last-sync"`
}
```

| Field | Type | Description |
| ----- | ---- | ----------- |
| Name  | UTF-8 String | The Name of the resource to monitor |
| Version | UTF-8 String | The version of that resource, an ETag |
| Data    | Binary data  | Arbitrary payload associated with this version |
| LastSync | 64 bit uint | The logical clock time associated with this write |

If you want something other than last-write wins you are probably trying to use
`vwatch` as a database, remember that is a bad idea.

## Notification protocol (Write)
To notify watchers of a new version of a resource, either the upstream source
of truth can respond with a new `ETag` on an already watched `name`, or an
external actor can `PUT` the new version directly.

**New upstream Version:** The `Leader` polling will generate a write with that
   node's local clock as the `last-sync` timestamp. Propagates as per `PUT`.

**`PUT` new Version**: The `Leader` that accepts the write, wakes all watchers
   on _different_ versions, and makes a best effort to `PUT` to peers.

New versions may specify their `version` via:
1. `ETag` version header (`vwatch` will strip `"`)
1. `version` URL parameter as a string.
2. Let the server calculate the version by `xxh3` digest of the body

New versions may specify their `last-sync` timestamp via:
1. `Last-Modified` header in RFC1123 GMT encoding
2. `modified` URL parameter in RFC3339 encoding
3. Let the server calculate the timestamp with current server time

The body conveys the `Data` of the version, which for `FILL_CACHE` Leaders
is excluded (get the data from the upstream, not `vwatch`).

An example of writing a version for the first time or updating via content:
```
PUT /v1/versions/{name}
123
```

Now updating that with a user provided timestamp
```
PUT /v1/versions/{name}?modified=2025-04-12T18:29:05-04:00
345
```
Now update with a user provided version and timestamp:
```
PUT /v1/versions/{name}?version=sha266:afb34...
678
```

## Watch protocol (Read)
To begin the protocol, a `GET` for the latest version of a `name` is issued
against any node:
```
GET /v1/versions/{name}
```

This will immediately respond with the current version as a standard `ETag`
header, for example `ETag: "sha256:edeaaff3f..."`.

To watch for changes to that `name`, set the version from that `GET` as the
`version` parameter on the same API, and the node will now block for up to
`timeout` (default=`10s`) waiting for a new version:
```
GET /v1/versions/{name}?version=sha256:edeaaff3f...
... blocks
```

The caller is responsible for keeping track of the version and re-issuing the
watch if they are still interested in the key.

## Watchers
While consuming applications can implement the Watch protocol, most deployments
would simply deploy `vwatch` as a sidecar process and let it Fill from the
read replicas. This gives local machine non blocking access to the latest
state (minus replication delay).

This layered approach makes `vwatch` easy to scale and integrate into any
application framework.

# Usefullness of this Architecture
While `vwatch` is not a database, it can be used to layer `Zookeeper`, `Etcd`
or `Consul` style watches on _other_ databases, which makes it quite useful.

## Monitoring an upstream Docker Registry
By configuring a `Leader` as follows, one can point `vwatch` at a Docker
manifest v2 compatible registry. If your image pushes webhook to the leaders
you can get faster notification:

```
VWATCH_FILL_FROM="https://<your docker manifst v2>:443"
VWATCH_FILL_PATH="/v2/{{.repository}}/manifests/{{.tag}}"
VWATCH_FILL_STRATEGY="FILL_CACHE"
VWATCH_FILL_HEADERS_MANIFEST="Accept: application/vnd.docker.distribution.manifest.v2+json"
VWATCH_FILL_HEADERS_FAT="Accept: application/vnd.docker.distribution.manifest.list.v2+json"
```

## Configuration Management
As each `Version` can store data (4KiB limit by default), `vwatch` can be used
to replicate small pieces of configuration ahead of a normal artifact system.
For example, one could store which node is primary or secondary and then use
`vwatch` to notify nodes that the primary has changed.

Note though, that you should always cache out the latest state to local disk
so that you can handle `vwatch` no longer being available.