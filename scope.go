package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Vertamedia/chproxy/cache"
	"github.com/Vertamedia/chproxy/config"
	"github.com/Vertamedia/chproxy/log"
	"github.com/prometheus/client_golang/prometheus"
)

type scopeID string

func newScopeID() scopeID {
	sid := atomic.AddUint64(&nextScopeID, 1)
	return scopeID(fmt.Sprintf("%08X", uint64(sid)))
}

var nextScopeID = uint64(time.Now().UnixNano())

type scope struct {
	startTime   time.Time
	id          scopeID
	host        *host
	cluster     *cluster
	user        *user
	clusterUser *clusterUser

	remoteAddr string
	localAddr  string

	// is true when KillQuery has been called
	canceled bool

	labels prometheus.Labels
}

func newScope(req *http.Request, u *user, c *cluster, cu *clusterUser) *scope {
	h := c.getHost()

	var localAddr string
	if addr, ok := req.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		localAddr = addr.String()
	}
	var id scopeID
	if val, ok := req.URL.Query()["query_id"]; ok {
		id = scopeID(val[0])
	} else {
		id = newScopeID()
	}
	s := &scope{
		startTime:   time.Now(),
		id:          id,
		host:        h,
		cluster:     c,
		user:        u,
		clusterUser: cu,

		remoteAddr: req.RemoteAddr,
		localAddr:  localAddr,

		labels: prometheus.Labels{
			"user":         u.name,
			"cluster":      c.name,
			"cluster_user": cu.name,
			"replica":      h.replica.name,
			"cluster_node": h.addr.Host,
		},
	}
	return s
}

func (s *scope) String() string {
	return fmt.Sprintf("[ Id: %s; User %q(%d) proxying as %q(%d) to %q(%d); RemoteAddr: %q; LocalAddr: %q; Duration: %d μs]",
		s.id,
		s.user.name, s.user.queryCounter.load(),
		s.clusterUser.name, s.clusterUser.queryCounter.load(),
		s.host.addr.Host, s.host.load(),
		s.remoteAddr, s.localAddr, time.Since(s.startTime).Nanoseconds()/1000.0)
}

func (s *scope) incQueued() error {
	if s.user.queueCh == nil && s.clusterUser.queueCh == nil {
		// Request queues in the current scope are disabled.
		return s.inc()
	}

	// Do not store `replica` and `cluster_node` in labels, since they have
	// no sense for queue metrics.
	labels := prometheus.Labels{
		"user":         s.labels["user"],
		"cluster":      s.labels["cluster"],
		"cluster_user": s.labels["cluster_user"],
	}

	if s.user.queueCh != nil {
		select {
		case s.user.queueCh <- struct{}{}:
			defer func() {
				<-s.user.queueCh
			}()
		default:
			// Per-user request queue is full.
			// Give the request the last chance to run.
			err := s.inc()
			if err != nil {
				userQueueOverflow.With(labels).Inc()
			}
			return err
		}
	}

	if s.clusterUser.queueCh != nil {
		select {
		case s.clusterUser.queueCh <- struct{}{}:
			defer func() {
				<-s.clusterUser.queueCh
			}()
		default:
			// Per-clusterUser request queue is full.
			// Give the request the last chance to run.
			err := s.inc()
			if err != nil {
				clusterUserQueueOverflow.With(labels).Inc()
			}
			return err
		}
	}

	// The request has been successfully queued.
	queueSize := requestQueueSize.With(labels)
	queueSize.Inc()
	defer queueSize.Dec()

	// Try starting the request during the given duration.
	d := s.maxQueueTime()
	dSleep := d / 10
	if dSleep > time.Second {
		dSleep = time.Second
	}
	if dSleep < time.Millisecond {
		dSleep = time.Millisecond
	}
	deadline := time.Now().Add(d)
	for {
		err := s.inc()
		if err == nil {
			// The request is allowed to start.
			return nil
		}

		dLeft := time.Until(deadline)
		if dLeft <= 0 {
			// Give up: the request exceeded its wait time
			// in the queue :(
			return err
		}

		// The request has dLeft remaining time to wait in the queue.
		// Sleep for a bit and try starting it again.
		if dSleep > dLeft {
			time.Sleep(dLeft)
		} else {
			time.Sleep(dSleep)
		}

		// Choose new host, since the previous one may become obsolete
		// after sleeping.
		h := s.cluster.getHost()
		s.host = h
		s.labels["replica"] = h.replica.name
		s.labels["cluster_node"] = h.addr.Host
	}
}

func (s *scope) inc() error {
	uQueries := s.user.queryCounter.inc()
	cQueries := s.clusterUser.queryCounter.inc()

	var err error
	if s.user.maxConcurrentQueries > 0 && uQueries > s.user.maxConcurrentQueries {
		err = fmt.Errorf("limits for user %q are exceeded: max_concurrent_queries limit: %d",
			s.user.name, s.user.maxConcurrentQueries)
	}
	if s.clusterUser.maxConcurrentQueries > 0 && cQueries > s.clusterUser.maxConcurrentQueries {
		err = fmt.Errorf("limits for cluster user %q are exceeded: max_concurrent_queries limit: %d",
			s.clusterUser.name, s.clusterUser.maxConcurrentQueries)
	}

	uRPM := s.user.rateLimiter.inc()
	cRPM := s.clusterUser.rateLimiter.inc()

	// int32(xRPM) > 0 check is required to detect races when RPM
	// is decremented on error below after per-minute zeroing
	// in rateLimiter.run.
	// These races become innocent with the given check.
	if s.user.reqPerMin > 0 && int32(uRPM) > 0 && uRPM > s.user.reqPerMin {
		err = fmt.Errorf("rate limit for user %q is exceeded: requests_per_minute limit: %d",
			s.user.name, s.user.reqPerMin)
	}
	if s.clusterUser.reqPerMin > 0 && int32(cRPM) > 0 && cRPM > s.clusterUser.reqPerMin {
		err = fmt.Errorf("rate limit for cluster user %q is exceeded: requests_per_minute limit: %d",
			s.clusterUser.name, s.clusterUser.reqPerMin)
	}

	if err != nil {
		s.user.queryCounter.dec()
		s.clusterUser.queryCounter.dec()

		// Decrement rate limiter here, so it doesn't count requests
		// that didn't start due to limits overflow.
		s.user.rateLimiter.dec()
		s.clusterUser.rateLimiter.dec()
		return err
	}

	s.host.inc()
	concurrentQueries.With(s.labels).Inc()
	return nil
}

func (s *scope) dec() {
	// There is no need in ratelimiter.dec here, since the rate limiter
	// is automatically zeroed every minute in rateLimiter.run.

	s.user.queryCounter.dec()
	s.clusterUser.queryCounter.dec()
	s.host.dec()
	concurrentQueries.With(s.labels).Dec()
}

const killQueryTimeout = time.Second * 30

func (s *scope) killQuery() error {
	log.Debugf("killing the query with query_id=%s", s.id)
	killedRequests.With(s.labels).Inc()
	s.canceled = true

	query := fmt.Sprintf("KILL QUERY WHERE query_id = '%s'", s.id)
	r := strings.NewReader(query)
	addr := s.host.addr.String()
	req, err := http.NewRequest("POST", addr, r)
	if err != nil {
		return fmt.Errorf("error while creating kill query request to %s: %s", addr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), killQueryTimeout)
	defer cancel()

	req = req.WithContext(ctx)

	// send request as kill_query_user
	userName := s.cluster.killQueryUserName
	if len(userName) == 0 {
		userName = "default"
	}
	req.SetBasicAuth(userName, s.cluster.killQueryUserPassword)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("error while executing clickhouse query %q at %q: %s", query, addr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		responseBody, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code returned from query %q at %q: %d. Response body: %q",
			query, addr, resp.StatusCode, responseBody)
	}

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("cannot read response body for the query %q: %s", query, err)
	}

	log.Debugf("killed the query with query_id=%s; respBody: %q", s.id, respBody)
	return nil
}

// allowedParams contains query args allowed to be proxied.
// See http://clickhouse-docs.readthedocs.io/en/latest/settings/
//
// All the other params passed via query args are stripped before
// proxying the request. This is for the sake of security.
var allowedParams = []string{
	"query",
	"database",
	"default_format",
	// if `compress=1`, CH will compress the data it sends you
	"compress",
	// if `decompress=1` , CH will decompress the same data that you pass in the POST method
	"decompress",
	// compress the result if the client over HTTP said that it understands data compressed by gzip or deflate.
	"enable_http_compression",
	// limit on the number of rows in the result
	"max_result_rows",
	// whether to count extreme values
	"extremes",
	// what to do if the volume of the result exceeds one of the limits
	"result_overflow_mode",
}

// This regexp must match params needed to describe a way to use external data
// @see https://clickhouse.yandex/docs/en/table_engines/external_data/
var externalDataParams = regexp.MustCompile(`(_types|_structure|_format)$`)

func (s *scope) decorateRequest(req *http.Request) (*http.Request, url.Values) {
	// Make new params to purify URL.
	params := make(url.Values)

	// Set user params
	if s.user.params != nil {
		for _, param := range s.user.params.params {
			params.Set(param.Key, param.Value)
		}
	}

	// Keep allowed params.
	origParams := req.URL.Query()
	for _, param := range allowedParams {
		val := origParams.Get(param)
		if len(val) > 0 {
			params.Set(param, val)
		}
	}

	// Keep external_data params
	if req.Method == "POST" {
		ct := req.Header.Get("Content-Type")
		if strings.Contains(ct, "multipart/form-data") {
			for key := range origParams {
				if externalDataParams.MatchString(key) {
					params.Set(key, origParams.Get(key))
				}
			}

			// disable cache for external_data queries
			origParams.Set("no_cache", "1")
			log.Debugf("external data params detected - cache will be disabled")
		}
	}

	// Set query_id as scope_id to have possibility to kill query if needed.
	params.Set("query_id", string(s.id))

	req.URL.RawQuery = params.Encode()

	// Rewrite possible previous Basic Auth and send request
	// as cluster user.
	req.SetBasicAuth(s.clusterUser.name, s.clusterUser.password)
	// Delete possible X-ClickHouse headers,
	// it is not allowed to use X-ClickHouse HTTP headers and other authentication methods simultaneously
	req.Header.Del("X-ClickHouse-User")
	req.Header.Del("X-ClickHouse-Key")

	// Send request to the chosen host from cluster.
	req.URL.Scheme = s.host.addr.Scheme
	req.URL.Host = s.host.addr.Host

	// Extend ua with additional info, so it may be queried
	// via system.query_log.http_user_agent.
	ua := fmt.Sprintf("RemoteAddr: %s; LocalAddr: %s; CHProxy-User: %s; CHProxy-ClusterUser: %s; %s",
		s.remoteAddr, s.localAddr, s.user.name, s.clusterUser.name, req.UserAgent())
	req.Header.Set("User-Agent", ua)

	return req, origParams
}

func (s *scope) getTimeoutWithErrMsg() (time.Duration, error) {
	var (
		timeout       time.Duration
		timeoutErrMsg error
	)
	if s.user.maxExecutionTime > 0 {
		timeout = s.user.maxExecutionTime
		timeoutErrMsg = fmt.Errorf("timeout for user %q exceeded: %v", s.user.name, timeout)
	}
	if timeout == 0 || (s.clusterUser.maxExecutionTime > 0 && s.clusterUser.maxExecutionTime < timeout) {
		timeout = s.clusterUser.maxExecutionTime
		timeoutErrMsg = fmt.Errorf("timeout for cluster user %q exceeded: %v", s.clusterUser.name, timeout)
	}
	return timeout, timeoutErrMsg
}

func (s *scope) maxQueueTime() time.Duration {
	d := s.user.maxQueueTime
	if d <= 0 || s.clusterUser.maxQueueTime > 0 && s.clusterUser.maxQueueTime < d {
		d = s.clusterUser.maxQueueTime
	}
	if d <= 0 {
		// Default queue time.
		d = 10 * time.Second
	}
	return d
}

type paramsRegistry struct {
	// key is a hashed concatenation of the params list
	key uint32

	params []config.Param
}

func newParamsRegistry(params []config.Param) (*paramsRegistry, error) {
	if len(params) == 0 {
		return nil, fmt.Errorf("params can't be empty")
	}
	h := fnv.New32a()
	for _, p := range params {
		str := fmt.Sprintf("%s=%s&", p.Key, p.Value)
		h.Write([]byte(str))
	}
	return &paramsRegistry{
		key:    h.Sum32(),
		params: params,
	}, nil
}

type user struct {
	name     string
	password string

	toCluster string
	toUser    string

	maxConcurrentQueries uint32
	queryCounter         counter

	maxExecutionTime time.Duration

	reqPerMin   uint32
	rateLimiter rateLimiter

	queueCh      chan struct{}
	maxQueueTime time.Duration

	allowedNetworks config.Networks

	denyHTTP  bool
	denyHTTPS bool
	allowCORS bool

	cache  *cache.Cache
	params *paramsRegistry
}

type usersProfile struct {
	cfg      []config.User
	clusters map[string]*cluster
	caches   map[string]*cache.Cache
	params   map[string]*paramsRegistry
}

func (up usersProfile) newUsers() (map[string]*user, error) {
	users := make(map[string]*user, len(up.cfg))
	for _, u := range up.cfg {
		if _, ok := users[u.Name]; ok {
			return nil, fmt.Errorf("duplicate config for user %q", u.Name)
		}
		tmpU, err := up.newUser(u)
		if err != nil {
			return nil, fmt.Errorf("cannot initialize user %q: %s", u.Name, err)
		}
		users[u.Name] = tmpU
	}
	return users, nil
}

func (up usersProfile) newUser(u config.User) (*user, error) {
	c, ok := up.clusters[u.ToCluster]
	if !ok {
		return nil, fmt.Errorf("unknown `to_cluster` %q", u.ToCluster)
	}
	if _, ok := c.users[u.ToUser]; !ok {
		return nil, fmt.Errorf("unknown `to_user` %q in cluster %q", u.ToUser, u.ToCluster)
	}

	var queueCh chan struct{}
	if u.MaxQueueSize > 0 {
		queueCh = make(chan struct{}, u.MaxQueueSize)
	}

	var cc *cache.Cache
	if len(u.Cache) > 0 {
		cc = up.caches[u.Cache]
		if cc == nil {
			return nil, fmt.Errorf("unknown `cache` %q", u.Cache)
		}
	}

	var params *paramsRegistry
	if len(u.Params) > 0 {
		params = up.params[u.Params]
		if params == nil {
			return nil, fmt.Errorf("unknown `params` %q", u.Params)
		}
	}

	return &user{
		name:                 u.Name,
		password:             u.Password,
		toCluster:            u.ToCluster,
		toUser:               u.ToUser,
		maxConcurrentQueries: u.MaxConcurrentQueries,
		maxExecutionTime:     time.Duration(u.MaxExecutionTime),
		reqPerMin:            u.ReqPerMin,
		queueCh:              queueCh,
		maxQueueTime:         time.Duration(u.MaxQueueTime),
		allowedNetworks:      u.AllowedNetworks,
		denyHTTP:             u.DenyHTTP,
		denyHTTPS:            u.DenyHTTPS,
		allowCORS:            u.AllowCORS,
		cache:                cc,
		params:               params,
	}, nil
}

type clusterUser struct {
	name     string
	password string

	maxConcurrentQueries uint32
	queryCounter         counter

	maxExecutionTime time.Duration

	reqPerMin   uint32
	rateLimiter rateLimiter

	queueCh      chan struct{}
	maxQueueTime time.Duration

	allowedNetworks config.Networks
}

func newClusterUser(cu config.ClusterUser) *clusterUser {
	var queueCh chan struct{}
	if cu.MaxQueueSize > 0 {
		queueCh = make(chan struct{}, cu.MaxQueueSize)
	}
	return &clusterUser{
		name:                 cu.Name,
		password:             cu.Password,
		maxConcurrentQueries: cu.MaxConcurrentQueries,
		maxExecutionTime:     time.Duration(cu.MaxExecutionTime),
		reqPerMin:            cu.ReqPerMin,
		queueCh:              queueCh,
		maxQueueTime:         time.Duration(cu.MaxQueueTime),
		allowedNetworks:      cu.AllowedNetworks,
	}
}

type host struct {
	replica *replica

	// Counter of unsuccessful requests to decrease host priority.
	penalty uint32

	// Either the current host is alive.
	active uint32

	// Host address.
	addr *url.URL

	counter
}

type replica struct {
	cluster *cluster

	name string

	hosts       []*host
	nextHostIdx uint32
}

func newReplicas(replicasCfg []config.Replica, nodes []string, scheme string, c *cluster) ([]*replica, error) {
	if len(nodes) > 0 {
		// No replicas, just flat nodes. Create default replica
		// containing all the nodes.
		r := &replica{
			cluster: c,
			name:    "default",
		}
		hosts, err := newNodes(nodes, scheme, r)
		if err != nil {
			return nil, err
		}
		r.hosts = hosts
		return []*replica{r}, nil
	}

	replicas := make([]*replica, len(replicasCfg))
	for i, rCfg := range replicasCfg {
		r := &replica{
			cluster: c,
			name:    rCfg.Name,
		}
		hosts, err := newNodes(rCfg.Nodes, scheme, r)
		if err != nil {
			return nil, fmt.Errorf("cannot initialize replica %q: %s", rCfg.Name, err)
		}
		r.hosts = hosts
		replicas[i] = r
	}
	return replicas, nil
}

func newNodes(nodes []string, scheme string, r *replica) ([]*host, error) {
	hosts := make([]*host, len(nodes))
	for i, node := range nodes {
		addr, err := url.Parse(fmt.Sprintf("%s://%s", scheme, node))
		if err != nil {
			return nil, fmt.Errorf("cannot parse `node` %q with `scheme` %q: %s", node, scheme, err)
		}
		hosts[i] = &host{
			replica: r,
			addr:    addr,
		}
	}
	return hosts, nil
}

func (h *host) runHeartbeat(done <-chan struct{}) {
	label := prometheus.Labels{
		"cluster":      h.replica.cluster.name,
		"replica":      h.replica.name,
		"cluster_node": h.addr.Host,
	}
	hb := h.replica.cluster.heartBeat
	heartbeat := func() {
		if err := hb.isHealthy(h.addr.String()); err == nil {
			atomic.StoreUint32(&h.active, uint32(1))
			hostHealth.With(label).Set(1)
		} else {
			log.Errorf("error while health-checking %q host: %s", h.addr.Host, err)
			atomic.StoreUint32(&h.active, uint32(0))
			hostHealth.With(label).Set(0)
		}
	}
	heartbeat()
	for {
		select {
		case <-done:
			return
		case <-time.After(hb.interval):
			heartbeat()
		}
	}
}

func (h *host) isActive() bool { return atomic.LoadUint32(&h.active) == 1 }

func (r *replica) isActive() bool {
	// The replica is active if at least a single host is active.
	for _, h := range r.hosts {
		if h.isActive() {
			return true
		}
	}
	return false
}

const (
	// prevents excess goroutine creating while penalizing overloaded host
	penaltySize     = 5
	penaltyMaxSize  = 300
	penaltyDuration = time.Second * 10
)

// decrease host priority for next requests
func (h *host) penalize() {
	p := atomic.LoadUint32(&h.penalty)
	if p >= penaltyMaxSize {
		return
	}
	hostPenalties.With(prometheus.Labels{
		"cluster":      h.replica.cluster.name,
		"replica":      h.replica.name,
		"cluster_node": h.addr.Host,
	}).Inc()
	atomic.AddUint32(&h.penalty, penaltySize)
	time.AfterFunc(penaltyDuration, func() {
		atomic.AddUint32(&h.penalty, ^uint32(penaltySize - 1))
	})
}

// overload runningQueries to take penalty into consideration
func (h *host) load() uint32 {
	c := h.counter.load()
	p := atomic.LoadUint32(&h.penalty)
	return c + p
}

func (r *replica) load() uint32 {
	var reqs uint32
	for _, h := range r.hosts {
		reqs += h.load()
	}
	return reqs
}

type cluster struct {
	name string

	replicas       []*replica
	nextReplicaIdx uint32

	users map[string]*clusterUser

	killQueryUserName     string
	killQueryUserPassword string

	heartBeat *heartBeat
}

func newCluster(c config.Cluster) (*cluster, error) {
	clusterUsers := make(map[string]*clusterUser, len(c.ClusterUsers))
	for _, cu := range c.ClusterUsers {
		if _, ok := clusterUsers[cu.Name]; ok {
			return nil, fmt.Errorf("duplicate config for cluster user %q", cu.Name)
		}
		clusterUsers[cu.Name] = newClusterUser(cu)
	}

	heartBeat := newHeartBeat(c.HeartBeat, c.ClusterUsers[0])

	newC := &cluster{
		name:                  c.Name,
		users:                 clusterUsers,
		killQueryUserName:     c.KillQueryUser.Name,
		killQueryUserPassword: c.KillQueryUser.Password,
		heartBeat:             heartBeat,
	}

	replicas, err := newReplicas(c.Replicas, c.Nodes, c.Scheme, newC)
	if err != nil {
		return nil, fmt.Errorf("cannot initialize replicas: %s", err)
	}
	newC.replicas = replicas

	return newC, nil
}

func newClusters(cfg []config.Cluster) (map[string]*cluster, error) {
	clusters := make(map[string]*cluster, len(cfg))
	for _, c := range cfg {
		if _, ok := clusters[c.Name]; ok {
			return nil, fmt.Errorf("duplicate config for cluster %q", c.Name)
		}
		tmpC, err := newCluster(c)
		if err != nil {
			return nil, fmt.Errorf("cannot initialize cluster %q: %s", c.Name, err)
		}
		clusters[c.Name] = tmpC
	}
	return clusters, nil
}

// getReplica returns least loaded + round-robin replica from the cluster.
//
// Always returns non-nil.
func (c *cluster) getReplica() *replica {
	idx := atomic.AddUint32(&c.nextReplicaIdx, 1)
	n := uint32(len(c.replicas))
	if n == 1 {
		return c.replicas[0]
	}

	idx %= n
	r := c.replicas[idx]
	reqs := r.load()

	// Set least priority to inactive replica.
	if !r.isActive() {
		reqs = ^uint32(0)
	}

	if reqs == 0 {
		return r
	}

	// Scan all the replicas for the least loaded replica.
	for i := uint32(1); i < n; i++ {
		tmpIdx := (idx + i) % n
		tmpR := c.replicas[tmpIdx]
		if !tmpR.isActive() {
			continue
		}
		tmpReqs := tmpR.load()
		if tmpReqs == 0 {
			return tmpR
		}
		if tmpReqs < reqs {
			r = tmpR
			reqs = tmpReqs
		}
	}

	// The returned replica may be inactive. This is OK,
	// since this means all the replicas are inactive,
	// so let's try proxying the request to any replica.
	return r
}

// getHost returns least loaded + round-robin host from replica.
//
// Always returns non-nil.
func (r *replica) getHost() *host {
	idx := atomic.AddUint32(&r.nextHostIdx, 1)
	n := uint32(len(r.hosts))
	if n == 1 {
		return r.hosts[0]
	}

	idx %= n
	h := r.hosts[idx]
	reqs := h.load()

	// Set least priority to inactive host.
	if !h.isActive() {
		reqs = ^uint32(0)
	}

	if reqs == 0 {
		return h
	}

	// Scan all the hosts for the least loaded host.
	for i := uint32(1); i < n; i++ {
		tmpIdx := (idx + i) % n
		tmpH := r.hosts[tmpIdx]
		if !tmpH.isActive() {
			continue
		}
		tmpReqs := tmpH.load()
		if tmpReqs == 0 {
			return tmpH
		}
		if tmpReqs < reqs {
			h = tmpH
			reqs = tmpReqs
		}
	}

	// The returned host may be inactive. This is OK,
	// since this means all the hosts are inactive,
	// so let's try proxying the request to any host.
	return h
}

// getHost returns least loaded + round-robin host from cluster.
//
// Always returns non-nil.
func (c *cluster) getHost() *host {
	r := c.getReplica()
	return r.getHost()
}

type rateLimiter struct {
	counter
}

func (rl *rateLimiter) run(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-time.After(time.Minute):
			rl.store(0)
		}
	}
}

type counter struct {
	value uint32
}

func (c *counter) store(n uint32) { atomic.StoreUint32(&c.value, n) }

func (c *counter) load() uint32 { return atomic.LoadUint32(&c.value) }

func (c *counter) dec() { atomic.AddUint32(&c.value, ^uint32(0)) }

func (c *counter) inc() uint32 { return atomic.AddUint32(&c.value, 1) }
