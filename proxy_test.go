package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/Vertamedia/chproxy/config"
)

const (
	okResponse = "1\n"
)

var goodCfg = &config.Config{
	Clusters: []config.Cluster{
		{
			Name:   "cluster",
			Scheme: "http",
			Replicas: []config.Replica{
				{
					Nodes: []string{"localhost:8123"},
				},
			},
			ClusterUsers: []config.ClusterUser{
				{
					Name: "web",
				},
			},
			HeartBeatInterval: config.Duration(time.Second * 5),
		},
	},
	Users: []config.User{
		{
			Name:      "default",
			ToCluster: "cluster",
			ToUser:    "web",
		},
	},
}

func newConfiguredProxy(cfg *config.Config) (*reverseProxy, error) {
	p := newReverseProxy()
	if err := p.applyConfig(cfg); err != nil {
		return p, fmt.Errorf("error while loading config: %s", err)
	}
	return p, nil
}

func TestNewReverseProxy(t *testing.T) {
	proxy := newReverseProxy()
	if err := proxy.applyConfig(goodCfg); err != nil {
		t.Fatalf("error while loading config: %s", err)
	}
	if len(proxy.clusters) != 1 {
		t.Fatalf("got %d hosts; expResponse: %d", len(proxy.clusters), 1)
	}
	c := proxy.clusters["cluster"]
	r := c.replicas[0]
	if len(r.hosts) != 1 {
		t.Fatalf("got %d hosts; expResponse: %d", len(r.hosts), 1)
	}
	if r.hosts[0].addr.Host != "localhost:8123" {
		t.Fatalf("got %s host; expResponse: %s", r.hosts[0].addr.Host, "localhost:8123")
	}
	if len(proxy.users) != 1 {
		t.Fatalf("got %d users; expResponse: %d", len(proxy.users), 1)
	}
	if _, ok := proxy.users["default"]; !ok {
		t.Fatalf("expected user %q to be present in users", "default")
	}
}

var badCfg = &config.Config{
	Clusters: []config.Cluster{
		{
			Name:   "badCfg",
			Scheme: "http",
			Nodes:  []string{"localhost:8123"},
			ClusterUsers: []config.ClusterUser{
				{
					Name: "default",
				},
			},
			HeartBeatInterval: config.Duration(time.Second * 5),
		},
	},
	Users: []config.User{
		{
			Name:      "default",
			ToCluster: "cluster",
			ToUser:    "foo",
		},
	},
}

func TestApplyConfig(t *testing.T) {
	proxy, err := newConfiguredProxy(goodCfg)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err = proxy.applyConfig(badCfg); err == nil {
		t.Fatalf("error expected; got nil")
	}
	if _, ok := proxy.clusters["badCfg"]; ok {
		t.Fatalf("bad config applied; expected previous config")
	}
}

var authCfg = &config.Config{
	Clusters: []config.Cluster{
		{
			Name:   "cluster",
			Scheme: "http",
			Nodes:  []string{"localhost:8123"},
			ClusterUsers: []config.ClusterUser{
				{
					Name:     "web",
					Password: "webpass",
				},
			},
			HeartBeatInterval: config.Duration(time.Second * 5),
		},
	},
	Users: []config.User{
		{
			Name:      "foo",
			Password:  "bar",
			ToCluster: "cluster",
			ToUser:    "web",
		},
	},
}

func TestReverseProxy_ServeHTTP1(t *testing.T) {
	testCases := []struct {
		cfg           *config.Config
		name          string
		expResponse   string
		expStatusCode int
		f             func(p *reverseProxy) *http.Response
	}{
		{
			cfg:           goodCfg,
			name:          "Ok response",
			expResponse:   okResponse,
			expStatusCode: http.StatusOK,
			f:             func(p *reverseProxy) *http.Response { return makeRequest(p) },
		},
		{
			cfg:           goodCfg,
			name:          "max concurrent queries for cluster user",
			expResponse:   "limits for cluster user \"web\" are exceeded: max_concurrent_queries limit: 1;",
			expStatusCode: http.StatusTooManyRequests,
			f: func(p *reverseProxy) *http.Response {
				p.clusters["cluster"].users["web"].maxConcurrentQueries = 1
				go makeHeavyRequest(p, time.Millisecond*200)
				time.Sleep(time.Millisecond * 10)
				return makeRequest(p)
			},
		},
		{
			cfg:           goodCfg,
			name:          "max time for cluster user",
			expResponse:   "timeout for cluster user \"web\" exceeded: 10ms",
			expStatusCode: http.StatusGatewayTimeout,
			f: func(p *reverseProxy) *http.Response {
				p.clusters["cluster"].users["web"].maxExecutionTime = time.Millisecond * 10
				return makeHeavyRequest(p, time.Millisecond*20)
			},
		},
		{
			cfg:           goodCfg,
			name:          "choose max time between users",
			expResponse:   "timeout for user \"default\" exceeded: 10ms",
			expStatusCode: http.StatusGatewayTimeout,
			f: func(p *reverseProxy) *http.Response {
				p.users["default"].maxExecutionTime = time.Millisecond * 10
				p.clusters["cluster"].users["web"].maxExecutionTime = time.Millisecond * 15
				return makeHeavyRequest(p, time.Millisecond*20)
			},
		},
		{
			cfg:           goodCfg,
			name:          "choose max time between users2",
			expResponse:   "timeout for cluster user \"web\" exceeded: 10ms",
			expStatusCode: http.StatusGatewayTimeout,
			f: func(p *reverseProxy) *http.Response {
				p.users["default"].maxExecutionTime = time.Millisecond * 15
				p.clusters["cluster"].users["web"].maxExecutionTime = time.Millisecond * 10
				return makeHeavyRequest(p, time.Millisecond*20)
			},
		},
		{
			cfg:           goodCfg,
			name:          "max concurrent queries for user",
			expResponse:   "limits for user \"default\" are exceeded: max_concurrent_queries limit: 1;",
			expStatusCode: http.StatusTooManyRequests,
			f: func(p *reverseProxy) *http.Response {
				p.users["default"].maxConcurrentQueries = 1
				go makeHeavyRequest(p, time.Millisecond*200)
				time.Sleep(time.Millisecond * 10)
				return makeRequest(p)
			},
		},
		{
			cfg:           goodCfg,
			name:          "identical concurrent queries for user",
			expResponse:   "1\n\n",
			expStatusCode: http.StatusOK,
			f: func(p *reverseProxy) *http.Response {
				p.users["default"].detectIdenticalConcurrentQueries = true
				p.users["default"].queueCh = make(chan struct{}, 1)
				go makeHeavyRequest(p, time.Millisecond*200)
				time.Sleep(time.Millisecond * 10)
				go makeHeavyRequest(p, time.Millisecond*200)
				time.Sleep(time.Millisecond * 10)
				go makeHeavyRequest(p, time.Millisecond*200)
				time.Sleep(time.Millisecond * 10)
				return makeHeavyRequest(p, time.Millisecond*200)
			},
		},
		{
			cfg:           goodCfg,
			name:          "queuing queries for user",
			expResponse:   okResponse,
			expStatusCode: http.StatusOK,
			f: func(p *reverseProxy) *http.Response {
				p.users["default"].maxConcurrentQueries = 1
				p.users["default"].queueCh = make(chan struct{}, 2)
				go makeHeavyRequest(p, time.Millisecond*20)
				time.Sleep(time.Millisecond * 10)
				return makeHeavyRequest(p, time.Millisecond*20)
			},
		},
		{
			cfg:           goodCfg,
			name:          "queuing queries for cluster user",
			expResponse:   okResponse,
			expStatusCode: http.StatusOK,
			f: func(p *reverseProxy) *http.Response {
				p.users["default"].maxConcurrentQueries = 1
				p.clusters["cluster"].users["web"].queueCh = make(chan struct{}, 2)
				go makeHeavyRequest(p, time.Millisecond*20)
				time.Sleep(time.Millisecond * 10)
				return makeHeavyRequest(p, time.Millisecond*20)
			},
		},
		{
			cfg:           goodCfg,
			name:          "queue overflow for user",
			expResponse:   "limits for user \"default\" are exceeded: max_concurrent_queries limit: 1",
			expStatusCode: http.StatusTooManyRequests,
			f: func(p *reverseProxy) *http.Response {
				p.users["default"].maxConcurrentQueries = 1
				p.users["default"].queueCh = make(chan struct{}, 1)
				go makeHeavyRequest(p, time.Millisecond*200)
				time.Sleep(time.Millisecond * 5)
				go makeHeavyRequest(p, time.Millisecond*200)
				time.Sleep(time.Millisecond * 5)
				return makeHeavyRequest(p, time.Millisecond*200)
			},
		},
		{
			cfg:           authCfg,
			name:          "disallow https",
			expResponse:   "user \"foo\" is not allowed to access via https",
			expStatusCode: http.StatusForbidden,
			f: func(p *reverseProxy) *http.Response {
				p.users["foo"].denyHTTPS = true
				req := httptest.NewRequest("POST", fakeServer.URL, nil)
				req.SetBasicAuth("foo", "bar")
				req.TLS = &tls.ConnectionState{
					Version:           tls.VersionTLS12,
					HandshakeComplete: true,
				}
				return makeCustomRequest(p, req)
			},
		},
		{
			cfg:           authCfg,
			name:          "basic auth ok",
			expResponse:   okResponse,
			expStatusCode: http.StatusOK,
			f: func(p *reverseProxy) *http.Response {
				req := httptest.NewRequest("POST", fakeServer.URL, nil)
				req.SetBasicAuth("foo", "bar")
				return makeCustomRequest(p, req)
			},
		},
		{
			cfg:           goodCfg,
			name:          "disallow http",
			expResponse:   "user \"default\" is not allowed to access via http",
			expStatusCode: http.StatusForbidden,
			f: func(p *reverseProxy) *http.Response {
				p.users["default"].denyHTTP = true
				return makeRequest(p)
			},
		},
		{
			cfg:           authCfg,
			name:          "basic auth wrong name",
			expResponse:   "invalid username or password for user \"fooo\"",
			expStatusCode: http.StatusUnauthorized,
			f: func(p *reverseProxy) *http.Response {
				req := httptest.NewRequest("POST", fakeServer.URL, nil)
				req.SetBasicAuth("fooo", "bar")
				return makeCustomRequest(p, req)
			},
		},
		{
			cfg:           authCfg,
			name:          "basic auth wrong pass",
			expResponse:   "invalid username or password for user \"foo\"",
			expStatusCode: http.StatusUnauthorized,
			f: func(p *reverseProxy) *http.Response {
				req := httptest.NewRequest("POST", fakeServer.URL, nil)
				req.SetBasicAuth("foo", "baar")
				return makeCustomRequest(p, req)
			},
		},
		{
			cfg:           authCfg,
			name:          "auth ok",
			expResponse:   okResponse,
			expStatusCode: http.StatusOK,
			f: func(p *reverseProxy) *http.Response {
				uri := fmt.Sprintf("%s?user=foo&password=bar", fakeServer.URL)
				req := httptest.NewRequest("POST", uri, nil)
				return makeCustomRequest(p, req)
			},
		},
		{
			cfg:           authCfg,
			name:          "auth wrong name",
			expResponse:   "invalid username or password for user \"fooo\"",
			expStatusCode: http.StatusUnauthorized,
			f: func(p *reverseProxy) *http.Response {
				uri := fmt.Sprintf("%s?user=fooo&password=bar", fakeServer.URL)
				req := httptest.NewRequest("POST", uri, nil)
				return makeCustomRequest(p, req)
			},
		},
		{
			cfg:           authCfg,
			name:          "auth wrong name",
			expResponse:   "invalid username or password for user \"foo\"",
			expStatusCode: http.StatusUnauthorized,
			f: func(p *reverseProxy) *http.Response {
				uri := fmt.Sprintf("%s?user=foo&password=baar", fakeServer.URL)
				req := httptest.NewRequest("POST", uri, nil)
				return makeCustomRequest(p, req)
			},
		},
		{
			cfg:           authCfg,
			name:          "headers auth ok",
			expResponse:   okResponse,
			expStatusCode: http.StatusOK,
			f: func(p *reverseProxy) *http.Response {
				req := httptest.NewRequest("POST", fakeServer.URL, nil)
				req.Header.Set("X-ClickHouse-User", "foo")
				req.Header.Set("X-ClickHouse-Key", "bar")
				return makeCustomRequest(p, req)
			},
		},
		{
			cfg:           authCfg,
			name:          "header auth wrong name",
			expResponse:   "invalid username or password for user \"fooo\"",
			expStatusCode: http.StatusUnauthorized,
			f: func(p *reverseProxy) *http.Response {
				req := httptest.NewRequest("POST", fakeServer.URL, nil)
				req.Header.Set("X-ClickHouse-User", "fooo")
				req.Header.Set("X-ClickHouse-Key", "bar")
				return makeCustomRequest(p, req)
			},
		},
		{
			cfg:           authCfg,
			name:          "header auth wrong name",
			expResponse:   "invalid username or password for user \"foo\"",
			expStatusCode: http.StatusUnauthorized,
			f: func(p *reverseProxy) *http.Response {
				req := httptest.NewRequest("POST", fakeServer.URL, nil)
				req.Header.Set("X-ClickHouse-User", "foo")
				req.Header.Set("X-ClickHouse-Key", "baar")
				return makeCustomRequest(p, req)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			proxy, err := getProxy(tc.cfg)
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			resp := tc.f(proxy)
			b := bbToString(t, resp.Body)
			resp.Body.Close()
			if !strings.Contains(b, tc.expResponse) {
				t.Fatalf("expected response: %q; got: %q", tc.expResponse, b)
			}
			if tc.expStatusCode != resp.StatusCode {
				t.Fatalf("unexpected status code: %d; expected: %d", resp.StatusCode, tc.expStatusCode)
			}
		})
	}

	t.Run("basicauth success", func(t *testing.T) {
		proxy, err := getProxy(authCfg)
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		req := httptest.NewRequest("POST", fakeServer.URL, nil)
		req.SetBasicAuth("foo", "bar")
		resp := makeCustomRequest(proxy, req)

		expected := okResponse
		b := bbToString(t, resp.Body)
		if !strings.Contains(b, expected) {
			t.Fatalf("expected response: %q; got: %q", expected, b)
		}
		resp.Body.Close()

		user, pass := getAuth(req)
		if user != authCfg.Clusters[0].ClusterUsers[0].Name {
			t.Fatalf("user name expected to be %q; got %q", authCfg.Clusters[0].ClusterUsers[0].Name, user)
		}
		if pass != authCfg.Clusters[0].ClusterUsers[0].Password {
			t.Fatalf("user password expected to be %q; got %q", authCfg.Clusters[0].ClusterUsers[0].Password, pass)
		}
	})

	t.Run("auth success", func(t *testing.T) {
		proxy, err := getProxy(authCfg)
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		uri := fmt.Sprintf("%s?user=foo&password=bar", fakeServer.URL)
		req := httptest.NewRequest("POST", uri, nil)
		resp := makeCustomRequest(proxy, req)

		expected := okResponse
		b := bbToString(t, resp.Body)
		if !strings.Contains(b, expected) {
			t.Fatalf("expected response: %q; got: %q", expected, b)
		}
		resp.Body.Close()

		user, pass := getAuth(req)
		if user != authCfg.Clusters[0].ClusterUsers[0].Name {
			t.Fatalf("user name expected to be %q; got %q", authCfg.Clusters[0].ClusterUsers[0].Name, user)
		}
		if pass != authCfg.Clusters[0].ClusterUsers[0].Password {
			t.Fatalf("user password expected to be %q; got %q", authCfg.Clusters[0].ClusterUsers[0].Password, pass)
		}
	})
}

func TestKillQuery(t *testing.T) {
	testCases := []struct {
		name string
		f    func(p *reverseProxy) *http.Response
	}{
		{
			name: "timeout user",
			f: func(p *reverseProxy) *http.Response {
				p.users["default"].maxExecutionTime = time.Millisecond * 10
				return makeHeavyRequest(p, time.Millisecond*20)
			},
		},
		{
			name: "timeout cluster user",
			f: func(p *reverseProxy) *http.Response {
				p.clusters["cluster"].users["web"].maxExecutionTime = time.Millisecond * 10
				return makeHeavyRequest(p, time.Millisecond*20)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			proxy, err := getProxy(goodCfg)
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			resp := tc.f(proxy)
			b := bbToString(t, resp.Body)
			id := extractID(scopeIDRe, b)
			if len(id) == 0 {
				t.Fatalf("expected Id to be extracted from %q", b)
			}

			time.Sleep(time.Millisecond * 30)
			state, err := registry.get(id)
			if err != nil {
				t.Fatalf("unexpected requestRegistry err for key %q: %s", id, err)
			}
			if !state {
				t.Fatalf("query expected to be killed; response: %s", b)
			}
		})
	}
}

func TestReverseProxy_ServeHTTPConcurrent(t *testing.T) {
	addr, err := url.Parse(fakeServer.URL)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	goodCfg.Clusters[0].Nodes = []string{addr.Host}
	proxy, err := newConfiguredProxy(goodCfg)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	t.Run("parallel requests", func(t *testing.T) {
		f := func() {
			req := httptest.NewRequest("GET", fmt.Sprintf("%s/fast", fakeServer.URL), nil)
			makeCustomRequest(proxy, req)
		}
		if err := testConcurrent(f, 500); err != nil {
			t.Fatalf("concurrent test err: %s", err)
		}
	})
	t.Run("parallel requests with config reloading", func(t *testing.T) {
		f := func() {
			proxy.applyConfig(newConfig())
			req := httptest.NewRequest("GET", fmt.Sprintf("%s/fast", fakeServer.URL), nil)
			makeCustomRequest(proxy, req)
		}
		if err := testConcurrent(f, 100); err != nil {
			t.Fatalf("concurrent test err: %s", err)
		}
	})
}

func TestReverseProxy_ServeHTTP2(t *testing.T) {
	testCases := []struct {
		name            string
		allowedNetworks config.Networks
		expResponse     string
	}{
		{
			name:            "empty allowed networks",
			allowedNetworks: config.Networks{},
		},
		{
			name:            "allow addr",
			allowedNetworks: config.Networks{getNetwork("192.0.2.1")},
		},
		{
			name:            "allow addr by mask",
			allowedNetworks: config.Networks{getNetwork("192.0.2.1/32")},
		},
	}

	f := func(cfg *config.Config) {
		proxy, err := getProxy(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		resp := makeRequest(proxy)
		b := bbToString(t, resp.Body)
		resp.Body.Close()
		if !strings.Contains(b, okResponse) {
			t.Fatalf("expected response: %q; got: %q", okResponse, b)
		}
	}

	for _, tc := range testCases {
		t.Run("user "+tc.name, func(t *testing.T) {
			goodCfg.Users[0].AllowedNetworks = tc.allowedNetworks
			f(goodCfg)
		})
		t.Run("cluster user "+tc.name, func(t *testing.T) {
			goodCfg.Clusters[0].ClusterUsers[0].AllowedNetworks = tc.allowedNetworks
			f(goodCfg)
		})
	}

	t.Run("cluster user disallow addr", func(t *testing.T) {
		goodCfg.Clusters[0].ClusterUsers[0].AllowedNetworks = config.Networks{getNetwork("192.0.2.2/32"), getNetwork("192.0.2.2")}
		proxy, err := getProxy(goodCfg)
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		resp := makeRequest(proxy)
		expected := "cluster user \"web\" is not allowed to access"
		b := bbToString(t, resp.Body)
		resp.Body.Close()
		if !strings.Contains(b, expected) {
			t.Fatalf("expected response: %q; got: %q", expected, b)
		}
	})

	t.Run("user disallow addr", func(t *testing.T) {
		goodCfg.Users[0].AllowedNetworks = config.Networks{getNetwork("192.0.2.2/32"), getNetwork("192.0.2.2")}
		proxy, err := getProxy(goodCfg)
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		resp := makeRequest(proxy)
		expected := "user \"default\" is not allowed to access"
		b := bbToString(t, resp.Body)
		resp.Body.Close()
		if !strings.Contains(b, expected) {
			t.Fatalf("expected response: %q; got: %q", expected, b)
		}
	})
}

func getNetwork(s string) *net.IPNet {
	if !strings.Contains(s, `/`) {
		s += "/32"
	}
	_, ipnet, _ := net.ParseCIDR(s)
	return ipnet
}

const killQueryPattern = "KILL QUERY WHERE query_id"

var (
	registry = newRequestRegistry()
	handler  = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fast" {
			fmt.Fprintln(w, okResponse)
			return
		}

		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
		b := string(body)
		r.Body.Close()

		qid := r.URL.Query().Get("query_id")
		if len(qid) == 0 && len(b) == 0 {
			// it's could be a health-check
			fmt.Fprintln(w, okResponse)
			return
		}
		registry.set(qid, false)
		if len(b) != 0 {
			if strings.Contains(b, killQueryPattern) {
				qid = extractID(queryIDRe, b)
				registry.set(qid, true)
			} else {
				d, err := time.ParseDuration(b)
				if err != nil {
					panic(fmt.Sprintf("error while imitating delay at fakeServer handler: %s", err))
				}
				time.Sleep(d)
			}
		}

		fmt.Fprintln(w, okResponse)
	})

	fakeServer = httptest.NewServer(handler)
)

func makeRequest(p *reverseProxy) *http.Response { return makeHeavyRequest(p, time.Duration(0)) }

func makeHeavyRequest(p *reverseProxy, duration time.Duration) *http.Response {
	body := bytes.NewBufferString(duration.String())
	req := httptest.NewRequest("POST", fakeServer.URL, body)
	return makeCustomRequest(p, req)
}

type testCloseNotifier struct {
	http.ResponseWriter
}

func (tcn *testCloseNotifier) CloseNotify() <-chan bool {
	return make(chan bool)
}

func makeCustomRequest(p *reverseProxy, req *http.Request) *http.Response {
	rw := httptest.NewRecorder()
	cn := &testCloseNotifier{rw}
	p.ServeHTTP(cn, req)
	return rw.Result()
}

func getProxy(c *config.Config) (*reverseProxy, error) {
	addr, err := url.Parse(fakeServer.URL)
	if err != nil {
		return nil, err
	}
	cfg := *c
	cfg.Clusters = make([]config.Cluster, len(c.Clusters))
	copy(cfg.Clusters, c.Clusters)
	cfg.Clusters[0].Nodes = []string{addr.Host}
	proxy, err := newConfiguredProxy(&cfg)
	if err != nil {
		return nil, err
	}

	// wait till all hosts will do health-checking
	time.Sleep(time.Millisecond * 50)
	return proxy, nil
}

func newConfig() *config.Config {
	newCfg := *goodCfg
	newCfg.Users = []config.User{
		{
			Name:                 "default",
			MaxConcurrentQueries: rand.Uint32(),
		},
	}
	return &newCfg
}

func bbToString(t *testing.T, r io.Reader) string {
	response, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected err while reading: %s", err)
	}
	return string(response)
}

var (
	scopeIDRe = regexp.MustCompile("Id: (.*?);")
	queryIDRe = regexp.MustCompile("'(.*?)'")
)

func extractID(re *regexp.Regexp, s string) string {
	subm := re.FindStringSubmatch(s)
	if len(subm) < 2 {
		return ""
	}
	return subm[1]
}

type requestRegistry struct {
	sync.Mutex
	// r is a registry of requests, where key is a query_id
	// and value is a state - was query killed(true) or not(false)
	r map[string]bool
}

func newRequestRegistry() *requestRegistry {
	return &requestRegistry{
		r: make(map[string]bool),
	}
}
func (r *requestRegistry) set(key string, v bool) {
	r.Lock()
	r.r[key] = v
	r.Unlock()
}

func (r *requestRegistry) get(key string) (bool, error) {
	r.Lock()
	defer r.Unlock()
	v, ok := r.r[key]
	if !ok {
		return false, fmt.Errorf("no such key")
	}
	return v, nil
}
