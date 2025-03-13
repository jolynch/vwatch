# vwatch
Watch versions and support high number of long pollers

# Build
This is a simple go binary, so all you need is a go toolchain and then:

```
go build
```

Your agent is now ready at `vwatch`

# Running
At its core `vwatch` let's you watch versions of named artifacts.

## Single Process Modes
The server is self describing HTTP server supporting simple PUT/GET operations
```bash
$ ./vwatch
2025/03/12 23:43:54 INFO Listening at 127.0.0.1:8080
2025/03/12 23:43:54 INFO Paths
GET /version/{repository}[:{tag}]?[version=last_seen]         -> Get latest version or block for new version
PUT /version/{repository}[:{tag}]?[version=version] <- <data> -> Set latest version, unblocking watches
PUT /logging?level=DEBUG                                      -> Set log level

Internal endpoints you should probably avoid unless you know what you are doing
POST /replicate                            <- gob([]Version) -> Replicate state between leaders
```

To set some data, call `PUT /version`, if you do not supply a version the crc32 of the data is used.
```bash
$ curl -XPUT localhost:8080/version/repo/artifact:latest -d '3456' -v
...
< HTTP/1.1 204 No Content
< Etag: 8d339230
< Last-Modified: Thu, 13 Mar 2025 03:48:12 GMT
< Date: Thu, 13 Mar 2025 03:48:12 GMT
```

Now you can get nonblocking via `GET /version`
```bash
$ curl -XGET 127.0.0.1:8080/version/repo/artifact:latest -v
< HTTP/1.1 200 OK
< Content-Type: application/octet-stream
< Etag: 8d339230
< Last-Modified: Thu, 13 Mar 2025 03:48:12 GMT
< Date: Thu, 13 Mar 2025 03:50:09 GMT
< Content-Length: 4
< 
* Connection #0 to host 127.0.0.1 left intact
3456
```

We can also block waiting for a new version
```bash
# GET
$ curl -XGET '127.0.0.1:8080/version/repo/artifact:latest?version=8d339230'
... blocks
```

If we then write to that version, all readers unblock with jitter
```bash
# PUT
$ curl -XPUT localhost:8080/version/repo/artifact:latest -d '1234'
```
The blocking read now unblocks with the new version
```bash
# GET

< HTTP/1.1 200 OK
< Content-Type: application/octet-stream
< Etag: 9be3e0a3
< Jittered: 689 ms
< Last-Modified: Thu, 13 Mar 2025 03:52:17 GMT
< Date: Thu, 13 Mar 2025 03:52:18 GMT
< Content-Length: 4
< 
* Connection #0 to host 127.0.0.1 left intact
1234
```

Note the new Etag, Last-Modified and Jittered header, as well as the new value.

## Multi Process Mode
You can have vwatch either `fill` from an upstream docker registry or vwatch process (allowing you to create read replicas),
or you can also have them `replicate` with each other to form a highly available write group. Let's all say it together though,
`vwatch` is not a database. It can fill from things that have a database, like a docker registry, S3, or an API server with versioned
resources, but `vwatch` just watches versions of named things. It does not store that data to disk, doesn't use paxos or raft,
and soley resolves conflicts with "that version is newer".

Example replication:
```bash
# First terminal
$ ./vwatch -listen 127.0.0.1:8080 --replicate-with http://127.0.0.2:8080
# Second agent
$ ./vwatch -listen 127.0.0.2:8080 --replicate-with http://127.0.0.1:8080
```

Now you can write to one, and read from either. You can also point it at a DNS name that contains all your IPs and those will
automatically be resolved and replicated with.