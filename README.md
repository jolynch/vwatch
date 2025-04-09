# vwatch
Watch versions and support high number of long pollers that are notified when versions change

Unlike something like `etcd`, this is not a database: it doesn't store data to disk, doesn't run
paxos or raft, and just generally isn't a database. Its goals are:

1. Scale to notifying millions of interested parties that a version has changed
2. In a best effort fashion minimize delay between write and notification

# Build
This is a simple go binary, so all you need is a go toolchain and then:

```
go build
```

Your agent is now ready at `vwatch`

# Running
At its core `vwatch` let's you watch versions of named artifacts. There are two ports, one for reads
(default `127.0.0.1:8080`) and one for writes (default `127.0.0.1:8008`). This is setup this
way so you can have different AuthN/AuthZ on these two parts of the API.

## Single Process Modes
The server is self describing HTTP server supporting simple PUT/GET operations
```bash
$ ./vwatch
2025/04/09 08:18:03 INFO Listening for Server traffic at 127.0.0.1:8008
2025/04/09 08:18:03 INFO Server HTTP Server Paths:
 PUT /v1/logging?level=DEBUG                            -> Set log level
POST /v1/versions                                       <- gob(VersionSet) | json(VersionSet) -> Replicate state between leaders
 PUT /v1/versions/{name}?[version={version}]            <- {data}                             -> Set latest version, unblocking watches

2025/04/09 08:18:03 INFO Listening for Client Traffic at 127.0.0.1:8080
2025/04/09 08:18:03 INFO Client HTTP Server Paths:
GET /v1/versions/{name}?[version={last_version}]        -> Get latest version or block for new version
```

To set some data, call `PUT /v1/versions`, if you do not supply a version the xxh3 of the data is used.
```bash
$ curl -XPUT localhost:8008/v1/versions/repo/artifact:latest -d '3456' -v
...
< HTTP/1.1 204 No Content
< Etag: "xxh3:ee66b703453af390404f394777116873"
< Last-Modified: Thu, 13 Mar 2025 03:48:12 GMT
< Date: Thu, 13 Mar 2025 03:48:12 GMT
```

Now you can get nonblocking via `GET /v1/versions` on the readable port
```bash
$ curl -XGET 127.0.0.1:8080/v1/versions/repo/artifact:latest -v
< HTTP/1.1 200 OK
< Content-Type: application/octet-stream
< Etag: "xxh3:ee66b703453af390404f394777116873"
< Last-Modified: Wed, 09 Apr 2025 15:19:20 GMT
< Server-Timing: watch;dur=0s
< Date: Wed, 09 Apr 2025 15:20:16 GMT
< Content-Length: 15
<
* Connection #0 to host 127.0.0.1 left intact
3456
```

We can also block waiting for a new version
```bash
# GET
$ curl -XGET '127.0.0.1:8080/v1/versions/repo/artifact:latest?version=xxh3:ee66b703453af390404f394777116873'
... blocks
```

If we then write to that version, all readers unblock with jitter
```bash
# PUT
$ curl -XPUT localhost:8008/v1/versions/repo/artifact:latest -d '1234'
```
The blocking read now unblocks with the new version
```bash
# GET

< HTTP/1.1 200 OK
< Content-Type: application/octet-stream
< Etag: "xxh3:9a4dea864648af82823c8c03e6dd2202"
< Last-Modified: Wed, 09 Apr 2025 15:25:15 GMT
< Server-Timing: jitter;dur=0s, watch;dur=2.671s
< Date: Wed, 09 Apr 2025 15:25:15 GMT
< Content-Length: 4
<
* Connection #0 to host 127.0.0.1 left intact
1234
```

Note the new Etag, Last-Modified and Server-Timing header, as well as the new value.

## Multi Process Mode
You can have vwatch either `fill` from an upstream docker registry or vwatch process (allowing you to create read replicas),
or you can also have them `replicate` with each other to form a highly available write group. Let's all say it together though,
`vwatch` is not a database. It can fill from things that have a database, like a docker registry, S3, or an API server with versioned
resources, but `vwatch` just watches versions of named things. It does not store that data to disk, doesn't use paxos or raft,
and solely resolves conflicts with "that version is newer".

Example replication:
```bash
# First terminal
$ ./vwatch -listen-client 127.0.0.1:8080 -listen-server 127.0.0.1:8008 --replicate-with http://127.0.0.2:8008
# Second agent
$ ./vwatch -listen-client 127.0.0.2:8080 -listen-server 127.0.0.2:8008 --replicate-with http://127.0.0.1:8008
```

Now you can write to one, and read from either. You can also point it at a DNS name that contains all your IPs and those will
automatically be resolved and replicated with (not smart enough to not try to replicate with itself).

## Watching an upstream docker registry

Filling with strategy CACHE allows you to fill from an upstream source like a docker registry v2 (anything that returns etags). You can use
`{{.name}}`, `{{.repostory}}` or `{{.tag}}` in your URL construction. Example with an active-active pair running against each other:

```bash
./vwatch -listen-client 127.0.0.1:8080 -listen-server 127.0.0.1:8008 -fill-addr "<registry>" -fill-path '/v2/{{.repository}}/manifests/{{.tag}}' -fill-strategy FILL_CACHE
```

Now the following will reach to the upstream registry and get the etag
```bash
curl -XGET '127.0.0.1:8080/version/repo/image:tag'
...
< Etag: "sha256:55109e781866b8bfb846e1f9ebf5da5b1db05be57ca0f005944b721706b79b92"
< Last-Modified: Fri, 14 Mar 2025 22:10:48 GMT
```

When running in FILL_CACHE mode, no data is stored, just the versions.