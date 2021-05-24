[![Go Report Card](https://goreportcard.com/badge/github.com/Vertamedia/chproxy)](https://goreportcard.com/report/github.com/Vertamedia/chproxy)
[![Build Status](https://travis-ci.org/Vertamedia/chproxy.svg?branch=master)](https://travis-ci.org/Vertamedia/chproxy?branch=master)
[![Coverage](https://img.shields.io/badge/gocover.io-75.7%25-green.svg)](http://gocover.io/github.com/Vertamedia/chproxy?version=1.9)

# chproxy

[English](README.md) | [简体中文](README-CN.md)

Chproxy, is an http proxy and load balancer for [ClickHouse](https://ClickHouse.yandex) database. It provides the following features:

- May proxy requests to multiple distinct `ClickHouse` clusters depending on the input user. For instance, requests from `appserver` user may go to `stats-raw` cluster, while requests from `reportserver` user may go to `stats-aggregate` cluster.
- May map input users to per-cluster users. This prevents from exposing real usernames and passwords used in `ClickHouse` clusters. Additionally this allows mapping multiple distinct input users to a single `ClickHouse` user.
- May accept incoming requests via HTTP and HTTPS.
- May limit HTTP and HTTPS access by IP/IP-mask lists.
- May limit per-user access by IP/IP-mask lists.
- May limit per-user query duration. Timed out or canceled queries are forcibly killed
  via [KILL QUERY](http://clickhouse-docs.readthedocs.io/en/latest/query_language/queries.html#kill-query).
- May limit per-user requests rate.
- May limit per-user number of concurrent requests.
- All the limits may be independently set for each input user and for each per-cluster user.
- May delay request execution until it fits per-user limits.
- Per-user [response caching](#caching) may be configured.
- Response caches have built-in protection against [thundering herd](https://en.wikipedia.org/wiki/Cache_stampede) problem aka `dogpile effect`.
- Evenly spreads requests among replicas and nodes using `least loaded` + `round robin` technique.
- Monitors node health and prevents from sending requests to unhealthy nodes.
- Supports automatic HTTPS certificate issuing and renewal via [Let’s Encrypt](https://letsencrypt.org/).
- May proxy requests to each configured cluster via either HTTP or [HTTPS](https://github.com/yandex/ClickHouse/blob/96d1ab89da451911eb54eccf1017eb5f94068a34/dbms/src/Server/config.xml#L15).
- Prepends User-Agent request header with remote/local address and in/out usernames before proxying it to `ClickHouse`, so this info may be queried from [system.query_log.http_user_agent](https://github.com/yandex/ClickHouse/issues/847).
- Exposes various useful [metrics](#metrics) in [prometheus text format](https://prometheus.io/docs/instrumenting/exposition_formats/).
- Configuration may be updated without restart - just send `SIGHUP` signal to `chproxy` process.
- Easy to manage and run - just pass config file path to a single `chproxy` binary.
- Easy to [configure](https://github.com/Vertamedia/chproxy/blob/master/config/examples/simple.yml):
```yml
server:
  http:
    listen_addr: ":9090"
    allowed_networks: ["127.0.0.0/24"]

users:
  - name: "default"
    to_cluster: "default"
    to_user: "default"

# by default each cluster has `default` user which can be overridden by section `users`
clusters:
  - name: "default"
    nodes: ["127.0.0.1:8123"]

```

## How to install

### Precompiled binaries

Precompiled `chproxy` binaries are available [here](https://github.com/Vertamedia/chproxy/releases).
Just download the latest stable binary, unpack and run it with the desired [config](#configuration):

```
./chproxy -config=/path/to/config.yml
```

### Building from source

Chproxy is written in [Go](https://golang.org/). The easiest way to install it from sources is:

```
go get -u github.com/Vertamedia/chproxy
```

If you don't have Go installed on your system - follow [this guide](https://golang.org/doc/install).


## Why it was created

`ClickHouse` may exceed [max_execution_time](http://clickhouse-docs.readthedocs.io/en/latest/settings/query_complexity.html#max-execution-time) and [max_concurrent_queries](https://github.com/yandex/ClickHouse/blob/add13f233eb6d30da4c75c4309542047a1dde033/dbms/src/Server/config.xml#L75) limits due to various reasons:
- `max_execution_time` may be exceeded due to the current [implementation deficiencies](https://github.com/yandex/ClickHouse/issues/217).
- `max_concurrent_queries` works only on a per-node basis. There is no way to limit the number of concurrent queries on a cluster if queries are spread across cluster nodes.

Such "leaky" limits may lead to high resource usage on all the cluster nodes. After facing this problem we had to maintain two distinct http proxies in front of our `ClickHouse` cluster - one for spreading `INSERT`s among cluster nodes and another one for sending `SELECT`s to a dedicated node where limits may be enforced somehow. This was fragile and inconvenient to manage, so `chproxy` has been created :)


## Use cases

### Spread `INSERT`s among cluster shards

Usually `INSERT`s are sent from app servers located in a limited number of subnetworks. `INSERT`s from other subnetworks must be denied.

All the `INSERT`s may be routed to a [distributed table](http://clickhouse-docs.readthedocs.io/en/latest/table_engines/distributed.html) on a single node. But this increases resource usage (CPU and network) on the node comparing to other nodes, since it must parse each row to be inserted and route it to the corresponding node (shard).

It would be better to spread `INSERT`s among available shards and to route them directly to per-shard tables instead of distributed tables. The routing logic may be embedded either directly into applications generating `INSERT`s or may be moved to a proxy. Proxy approach is better since it allows re-configuring `ClickHouse` cluster without modification of application configs and without application downtime. Multiple identical proxies may be started on distinct servers for scalability and availability purposes.

The following minimal `chproxy` config may be used for [this use case](https://github.com/Vertamedia/chproxy/blob/master/config/examples/spread.inserts.yml):
```yml
server:
  http:
      listen_addr: ":9090"

      # Networks with application servers.
      allowed_networks: ["10.10.1.0/24"]

users:
  - name: "insert"
    to_cluster: "stats-raw"
    to_user: "default"

clusters:
  - name: "stats-raw"

    # Requests are spread in `round-robin` + `least-loaded` fashion among nodes.
    # Unreachable and unhealthy nodes are skipped.
    nodes: [
      "10.10.10.1:8123",
      "10.10.10.2:8123",
      "10.10.10.3:8123",
      "10.10.10.4:8123"
    ]
```

### Spread `SELECT`s from reporting apps among cluster nodes

Reporting apps usually generate various customer reports from `SELECT` query results.
The load generated by such `SELECT`s on `ClickHouse` cluster may vary depending
on the number of online customers and on the generated report types. It is obvious
that the load must be limited in order to prevent cluster overload.

All the `SELECT`s may be routed to a [distributed table](http://clickhouse-docs.readthedocs.io/en/latest/table_engines/distributed.html) on a single node. But this increases resource usage (RAM, CPU and network) on the node comparing to other nodes, since it must do final aggregation, sorting and filtering for the data obtained from cluster nodes (shards).

It would be better to create identical distributed tables on each shard and spread `SELECT`s among all the available shards.

The following minimal `chproxy` config may be used for [this use case](https://github.com/Vertamedia/chproxy/blob/master/config/examples/spread.selects.yml):
```yml
server:
  http:
      listen_addr: ":9090"

      # Networks with reporting servers.
      allowed_networks: ["10.10.2.0/24"]

users:
  - name: "report"
    to_cluster: "stats-aggregate"
    to_user: "readonly"
    max_concurrent_queries: 6
    max_execution_time: 1m

clusters:
  - name: "stats-aggregate"
    nodes: [
      "10.10.20.1:8123",
      "10.10.20.2:8123"
    ]
    users:
      - name: "readonly"
        password: "****"
```

### Authorize users by passwords via HTTPS

Suppose you need to access `ClickHouse` cluster from anywhere by username/password.
This may be used for building graphs from [ClickHouse-grafana](https://github.com/Vertamedia/ClickHouse-grafana) or [tabix](https://tabix.io/).
It is bad idea to transfer unencrypted password and data over untrusted networks.
So HTTPS must be used for accessing the cluster in such cases.
The following `chproxy` config may be used for [this use case](https://github.com/Vertamedia/chproxy/blob/master/config/examples/https.yml):
```yml
server:
  https:
    listen_addr: ":443"
    autocert:
      cache_dir: "certs_dir"

users:
  - name: "web"
    password: "****"
    to_cluster: "stats-raw"
    to_user: "web"
    max_concurrent_queries: 2
    max_execution_time: 30s
    requests_per_minute: 10
    deny_http: true

    # Allow `CORS` requests for `tabix`.
    allow_cors: true

    # Enable requests queueing - `chproxy` will queue up to `max_queue_size`
    # of incoming requests for up to `max_queue_time` until they stop exceeding
    # the current limits.
    # This allows gracefully handling request bursts when more than
    # `max_concurrent_queries` concurrent requests arrive.
    max_queue_size: 40
    max_queue_time: 25s

    # Enable response caching. See cache config below.
    cache: "shortterm"

clusters:
  - name: "stats-raw"
    nodes: [
     "10.10.10.1:8123",
     "10.10.10.2:8123",
     "10.10.10.3:8123",
     "10.10.10.4:8123"
    ]
    users:
      - name: "web"
        password: "****"

caches:
  - name: "shortterm"
    dir: "/path/to/cache/dir"
    max_size: 150Mb

    # Cached responses will expire in 130s.
    expire: 130s
```

### All the above configs combined

All the above cases may be combined in a single `chproxy` [config](https://github.com/Vertamedia/chproxy/blob/master/config/examples/combined.yml):

```yml
server:
  http:
      listen_addr: ":9090"
      allowed_networks: ["10.10.1.0/24","10.10.2.0/24"]
  https:
    listen_addr: ":443"
    autocert:
      cache_dir: "certs_dir"

users:
  - name: "insert"
    allowed_networks: ["10.10.1.0/24"]
    to_cluster: "stats-raw"
    to_user: "default"

  - name: "report"
    allowed_networks: ["10.10.2.0/24"]
    to_cluster: "stats-aggregate"
    to_user: "readonly"
    max_concurrent_queries: 6
    max_execution_time: 1m

  - name: "web"
    password: "****"
    to_cluster: "stats-raw"
    to_user: "web"
    max_concurrent_queries: 2
    max_execution_time: 30s
    requests_per_minute: 10
    deny_http: true
    allow_cors: true
    max_queue_size: 40
    max_queue_time: 25s
    cache: "shortterm"

clusters:
  - name: "stats-aggregate"
    nodes: [
      "10.10.20.1:8123",
      "10.10.20.2:8123"
    ]
    users:
    - name: "readonly"
      password: "****"

  - name: "stats-raw"
    nodes: [
     "10.10.10.1:8123",
     "10.10.10.2:8123",
     "10.10.10.3:8123",
     "10.10.10.4:8123"
    ]
    users:
      - name: "default"

      - name: "web"
        password: "****"

caches:
  - name: "shortterm"
    dir: "/path/to/cache/dir"
    max_size: 150Mb
    expire: 130s
```

## Configuration

### Server
`Chproxy` may accept requests over `HTTP` and `HTTPS` protocols. [HTTPS](https://github.com/Vertamedia/chproxy/blob/master/config#https_config) must be configured with custom certificate or with automated [Let's Encrypt](https://letsencrypt.org/) certificates.

Access to `chproxy` can be limitied by list of IPs or IP masks. This option can be applied to [HTTP](https://github.com/Vertamedia/chproxy/blob/master/config#http_config), [HTTPS](https://github.com/Vertamedia/chproxy/blob/master/config#https_config), [metrics](https://github.com/Vertamedia/chproxy/blob/master/config#metrics_config), [user](https://github.com/Vertamedia/chproxy/blob/master/config#user_config) or [cluster-user](https://github.com/Vertamedia/chproxy/blob/master/config#cluster_user_config).

### Users
There are two types of users: `in-users` (in global section) and `out-users` (in cluster section).
This means all requests will be matched to `in-users` and if all checks are Ok - will be matched to `out-users`
with overriding credentials.

Suppose we have one ClickHouse user `web` with `read-only` permissions and `max_concurrent_queries: 4` limit.
There are two distinct applications `reading` from ClickHouse. We may create two distinct `in-users` with `to_user: "web"` and `max_concurrent_queries: 2` each in order to avoid situation when a single application exhausts all the 4-request limit on the `web` user.

Requests to `chproxy` must be authorized with credentials from [user_config](https://github.com/Vertamedia/chproxy/blob/master/config#user_config). Credentials can be passed via [BasicAuth](https://en.wikipedia.org/wiki/Basic_access_authentication) or via `user` and `password` [query string](https://en.wikipedia.org/wiki/Query_string) args.

Limits for `in-users` and `out-users` are independent.

### Clusters
`Chproxy` can be configured with multiple `cluster`s. Each `cluster` must have a name and either a list of nodes
or a list of replicas with nodes. See [cluster-config](https://github.com/Vertamedia/chproxy/tree/master/config#cluster_config) for details.
Requests to each cluster are balanced among replicas and nodes using `round-robin` + `least-loaded` approach.
The node priority is automatically decreased for a short interval if recent requests to it were unsuccessful.
This means that the `chproxy` will choose the next least loaded healthy node among least loaded replica
for every new request.

Additionally each node is periodically checked for availability. Unavailable nodes are automatically excluded from the cluster until they become available again. This allows performing node maintenance without removing unavailable nodes from the cluster config.

`Chproxy` automatically kills queries exceeding `max_execution_time` limit. By default `chproxy` tries to kill such queries
under `default` user. The user may be overriden with [kill_query_user](https://github.com/Vertamedia/chproxy/blob/master/config#kill_query_user_config).

If `cluster`'s [users](https://github.com/Vertamedia/chproxy/blob/master/config#cluster_user_config) section isn't specified, then `default` user is used with no limits.

### Caching

`Chproxy` may be configured to cache responses. It is possible to create multiple
[cache-configs](https://github.com/Vertamedia/chproxy/blob/master/config/#cache_config) with various settings.
Response caching is enabled by assigning cache name to user. Multiple users may share the same cache.
Currently only `SELECT` responses are cached.
Caching is disabled for request with `no_cache=1` in query string.
Optional cache namespace may be passed in query string as `cache_namespace=aaaa`. This allows caching
distinct responses for the identical query under distinct cache namespaces. Additionally,
an instant cache flush may be built on top of cache namespaces - just switch to new namespace in order
to flush the cache.

### Security
`Chproxy` removes all the query params from input requests (except the user's [params](https://github.com/Vertamedia/chproxy/blob/master/config#param_groups_config) and listed [here](https://github.com/Vertamedia/chproxy/blob/master/scope.go#L292))
before proxying them to `ClickHouse` nodes. This prevents from unsafe overriding
of various `ClickHouse` [settings](http://clickhouse-docs.readthedocs.io/en/latest/interfaces/http_interface.html).

Be careful when configuring limits, allowed networks, passwords etc.
By default `chproxy` tries detecting the most obvious configuration errors such as `allowed_networks: ["0.0.0.0/0"]` or sending passwords via unencrypted HTTP.

Special option `hack_me_please: true` may be used for disabling all the security-related checks during config validation (if you are feeling lucky :) ).

#### Example of [full](https://github.com/Vertamedia/chproxy/blob/master/config/testdata/full.yml) configuration:
```yml
# Whether to print debug logs.
#
# By default debug logs are disabled.
log_debug: true

# Whether to ignore security checks during config parsing.
#
# By default security checks are enabled.
hack_me_please: true

# Optional response cache configs.
#
# Multiple distinct caches with different settings may be configured.
caches:
    # Cache name, which may be passed into `cache` option on the `user` level.
    #
    # Multiple users may share the same cache.
  - name: "longterm"

    # Path to directory where cached responses will be stored.
    dir: "/path/to/longterm/cachedir"

    # Maximum cache size.
    # `Kb`, `Mb`, `Gb` and `Tb` suffixes may be used.
    max_size: 100Gb

    # Expiration time for cached responses.
    expire: 1h

    # When multiple requests with identical query simultaneously hit `chproxy`
    # and there is no cached response for the query, then only a single
    # request will be proxied to clickhouse. Other requests will wait
    # for the cached response during this grace duration.
    # This is known as protection from `thundering herd` problem.
    #
    # By default `grace_time` is 5s. Negative value disables the protection
    # from `thundering herd` problem.
    grace_time: 20s

  - name: "shortterm"
    dir: "/path/to/shortterm/cachedir"
    max_size: 100Mb
    expire: 10s

# Optional network lists, might be used as values for `allowed_networks`.
network_groups:
  - name: "office"
    # Each item may contain either IP or IP subnet mask.
    networks: ["127.0.0.0/24", "10.10.0.1"]

  - name: "reporting-apps"
    networks: ["10.10.10.0/24"]

# Optional lists of query params to send with each proxied request to ClickHouse.
# These lists may be used for overriding ClickHouse settings on a per-user basis.
param_groups:
    # Group name, which may be passed into `params` option on the `user` level.
  - name: "cron-job"
    # List of key-value params to send
    params:
      - key: "max_memory_usage"
        value: "40000000000"

      - key: "max_bytes_before_external_group_by"
        value: "20000000000"

  - name: "web"
    params:
      - key: "max_memory_usage"
        value: "5000000000"

      - key: "max_columns_to_read"
        value: "30"

      - key: "max_execution_time"
        value: "30"

# Settings for `chproxy` input interfaces.
server:
  # Configs for input http interface.
  # The interface works only if this section is present.
  http:
    # TCP address to listen to for http.
    # May be in the form IP:port . IP part is optional.
    listen_addr: ":9090"

    # List of allowed networks or network_groups.
    # Each item may contain IP address, IP subnet mask or a name
    # from `network_groups`.
    # By default requests are accepted from all the IPs.
    allowed_networks: ["office", "reporting-apps", "1.2.3.4"]

    # ReadTimeout is the maximum duration for proxy to reading the entire
    # request, including the body.
    # Default value is 1m
    read_timeout: 5m

    # WriteTimeout is the maximum duration for proxy before timing out writes of the response.
    # Default is largest MaxExecutionTime + MaxQueueTime value from Users or Clusters
    write_timeout: 10m

    # IdleTimeout is the maximum amount of time for proxy to wait for the next request.
    # Default is 10m
    idle_timeout: 20m

  # Configs for input https interface.
  # The interface works only if this section is present.
  https:
    # TCP address to listen to for https.
    listen_addr: ":443"

    # Paths to TLS cert and key files.
    # cert_file: "cert_file"
    # key_file: "key_file"

    # Letsencrypt config.
    # Certificates are automatically issued and renewed if this section
    # is present.
    # There is no need in cert_file and key_file if this section is present.
    # Autocert requires application to listen on :80 port for certificate generation
    autocert:
      # Path to the directory where autocert certs are cached.
      cache_dir: "certs_dir"

      # The list of host names proxy is allowed to respond to.
      # See https://godoc.org/golang.org/x/crypto/acme/autocert#HostPolicy
      allowed_hosts: ["example.com"]

  # Metrics in prometheus format are exposed on the `/metrics` path.
  # Access to `/metrics` endpoint may be restricted in this section.
  # By default access to `/metrics` is unrestricted.
  metrics:
    allowed_networks: ["office"]

# Configs for input users.
users:
    # Name and password are used to authorize access via BasicAuth or
    # via `user`/`password` query params.
    # Password is optional. By default empty password is used.
  - name: "web"
    password: "****"

    # Requests from the user are routed to this cluster.
    to_cluster: "first cluster"

    # Input user is substituted by the given output user from `to_cluster`
    # before proxying the request.
    to_user: "web"

    # Whether to deny input requests over HTTP.
    deny_http: true

    # Whether to allow `CORS` requests like `tabix` does.
    # By default `CORS` requests are denied for security reasons.
    allow_cors: true

    # Requests per minute limit for the given input user.
    #
    # By default there is no per-minute limit.
    requests_per_minute: 4

    # Response cache config name to use.
    #
    # By default responses aren't cached.
    cache: "longterm"

    # An optional group of params to send to ClickHouse with each proxied request.
    # These params may be set in param_groups block.
    #
    # By default no additional params are sent to ClickHouse.
    params: "web"

    # The maximum number of requests that may wait for their chance
    # to be executed because they cannot run now due to the current limits.
    #
    # This option may be useful for handling request bursts from `tabix`
    # or `clickhouse-grafana`.
    #
    # By default all the requests are immediately executed without
    # waiting in the queue.
    max_queue_size: 100

    # The maximum duration the queued requests may wait for their chance
    # to be executed.
    # This option makes sense only if max_queue_size is set.
    # By default requests wait for up to 10 seconds in the queue.
    max_queue_time: 35s

    # Whether to detect identical concurrent queries and run one query at time. 
    # All identical queries will decrease queue by only 1 in case if `max_queue_size` defined
    # Make sense when cache enabled
    #
    # By default its false
    detect_identical_concurrent_queries: false

  - name: "default"
    to_cluster: "second cluster"
    to_user: "default"
    allowed_networks: ["office", "1.2.3.0/24"]

    # The maximum number of concurrently running queries for the user.
    #
    # By default there is no limit on the number of concurrently
    # running queries.
    max_concurrent_queries: 4

    # The maximum query duration for the user.
    # Timed out queries are forcibly killed via `KILL QUERY`.
    #
    # By default there is no limit on the query duration.
    max_execution_time: 1m

    # Whether to deny input requests over HTTPS.
    deny_https: true

# Configs for ClickHouse clusters.
clusters:
    # The cluster name is used in `to_cluster`.
  - name: "first cluster"

    # Protocol to use for communicating with cluster nodes.
    # Currently supported values are `http` or `https`.
    # By default `http` is used.
    scheme: "http"

    # Cluster node addresses.
    # Requests are evenly distributed among them.
    nodes: ["127.0.0.1:8123", "shard2:8123"]

    # DEPRECATED: Each cluster node is checked for availability using this interval.
    # By default each node is checked for every 5 seconds.
    # Use `heartbeat.interval`.
    heartbeat_interval: 1m

    # User configuration for heart beat requests.
    # Credentials of the first user in clusters.users will be used for heart beat requests to clickhouse.
    heartbeat:
      # An interval for checking all cluster nodes for availability
      # By default each node is checked for every 5 seconds.
      interval: 1m

      # A timeout of wait response from cluster nodes
      # By default 3s
      timeout: 10s

      # The parameter to set the URI to request in a health check
      # By default "/?query=SELECT%201"
      request: "/?query=SELECT%201%2B1"

      # Reference response from clickhouse on health check request
      # By default "1\n"
      response: "2\n"

    # Timed out queries are killed using this user.
    # By default `default` user is used.
    kill_query_user:
      name: "default"
      password: "***"

    # Configuration for cluster users.
    users:
        # The user name is used in `to_user`.
      - name: "web"
        password: "password"
        max_concurrent_queries: 4
        max_execution_time: 1m

  - name: "second cluster"
    scheme: "https"

    # The cluster may contain multiple replicas instead of flat nodes.
    #
    # Chproxy selects the least loaded node among the least loaded replicas.
    replicas:
      - name: "replica1"
        nodes: ["127.0.1.1:8443", "127.0.1.2:8443"]
      - name: "replica2"
        nodes: ["127.0.2.1:8443", "127.0.2.2:8443"]

    users:
      - name: "default"
        max_concurrent_queries: 4
        max_execution_time: 1m

      - name: "web"
        max_concurrent_queries: 4
        max_execution_time: 10s
        requests_per_minute: 10
        max_queue_size: 50
        max_queue_time: 70s
        allowed_networks: ["office"]
```

#### Full specification is located [here](https://github.com/Vertamedia/chproxy/blob/master/config)

## Metrics
Metrics are exposed in [prometheus text format](https://prometheus.io/docs/instrumenting/exposition_formats/) at `/metrics` path.

| Name | Type | Description | Labels |
| ------------- | ------------- | ------------- | ------------- |
| bad_requests_total | Counter | The number of unsupported requests | |
| cache_hits_total | Counter | The amount of cache hits | `cache`, `user`, `cluster`, `cluster_user` |
| cache_items | Gauge | The number of items in each cache | `cache` |
| cache_miss_total | Counter | The amount of cache misses | `cache`, `user`, `cluster`, `cluster_user` |
| cache_size | Gauge | Size of each cache | `cache` |
| cached_response_duration_seconds | Summary | Duration for cached responses. Includes the duration for sending response to client | `cache`, `user`, `cluster`, `cluster_user` |
| canceled_request_total | Counter | The number of requests canceled by remote client | `user`, `cluster`, `cluster_user`, `replica`, `cluster_node` |
| cluster_user_queue_overflow_total | Counter | The number of overflows for per-cluster_user request queues | `user`, `cluster`, `cluster_user` |
| concurrent_limit_excess_total | Counter | The number of rejected requests due to max_concurrent_queries limit | `user`, `cluster`, `cluster_user`, `replica`, `cluster_node` |
| concurrent_queries | Gauge | The number of concurrent queries at the moment | `user`, `cluster`, `cluster_user`, `replica`, `cluster_node` |
| config_last_reload_successful | Gauge | Whether the last configuration reload attempt was successful | |
| config_last_reload_success_timestamp_seconds | Gauge | Timestamp of the last successful configuration reload | |
| host_health | Gauge | Health state of hosts by clusters | `cluster`, `replica`, `cluster_node` |
| host_penalties_total | Counter | The number of given penalties by host | `cluster`, `replica`, `cluster_node` |
| killed_request_total | Counter | The number of requests killed by proxy | `user`, `cluster`, `cluster_user`, `replica`, `cluster_node` |
| proxied_response_duration_seconds | Summary | Duration for responses proxied from clickhouse | `user`, `cluster`, `cluster_user`, `replica`, `cluster_node` |
| request_body_bytes_total | Counter | The amount of bytes read from request bodies | `user`, `cluster`, `cluster_user`, `replica`, `cluster_node` |
| request_duration_seconds | Summary | Request duration. Includes possible queue wait time | `user`, `cluster`, `cluster_user`, `replica`, `cluster_node` |
| request_queue_size | Gauge | Request queue size at the moment | `user`, `cluster`, `cluster_user` |
| request_success_total | Counter | The number of successfully proxied requests | `user`, `cluster`, `cluster_user`, `replica`, `cluster_node` |
| request_sum_total | Counter | The number of processed requests | `user`, `cluster`, `cluster_user`, `replica`, `cluster_node` |
| response_body_bytes_total | Counter | The amount of bytes written to response bodies | `user`, `cluster`, `cluster_user`, `replica`, `cluster_node` |
| status_codes_total | Counter | Distribution by response status codes | `user`, `cluster`, `cluster_user`, `replica`, `cluster_node`, `code` |
| timeout_request_total | Counter | The number of timed out requests | `user`, `cluster`, `cluster_user`, `replica`, `cluster_node` |
| user_queue_overflow_total | Counter | The number of overflows for per-user request queues | `user`, `cluster`, `cluster_user` |

An example of [Grafana's](https://grafana.com) dashboard for `chproxy` metrics is available [here](https://github.com/Vertamedia/chproxy/blob/master/chproxy_overview.json)

![dashboard example](https://user-images.githubusercontent.com/2902918/31392734-b2fd4a18-ade2-11e7-84a9-4aaaac4c10d7.png)


## FAQ

* *Is `chproxy` production ready?*

  Yes, we successfully use it in production for both `INSERT` and `SELECT`
  requests.

* *What about `chproxy` performance?*

  A single `chproxy` instance easily proxies 1Gbps of compressed `INSERT` data
  while using less than 20% of a single CPU core in our production setup.

* *Does `chproxy` support [native interface](http://clickhouse-docs.readthedocs.io/en/latest/interfaces/tcp.html) for ClickHouse?*

  No. Because currently all our services work with ClickHouse only via HTTP.
  Support for `native interface` may be added in the future.
