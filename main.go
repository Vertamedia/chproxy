package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Vertamedia/chproxy/config"
	"github.com/Vertamedia/chproxy/log"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/acme/autocert"
)

var (
	configFile = flag.String("config", "", "Proxy configuration filename")
	version    = flag.Bool("version", false, "Prints current version and exits")
)

var (
	proxy = newReverseProxy()

	// networks allow lists
	allowedNetworksHTTP    atomic.Value
	allowedNetworksHTTPS   atomic.Value
	allowedNetworksMetrics atomic.Value
)

func main() {
	flag.Parse()
	if *version {
		fmt.Printf("%s\n", versionString())
		os.Exit(0)
	}

	log.Infof("%s", versionString())
	log.Infof("Loading config: %s", *configFile)
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("error while loading config: %s", err)
	}
	if err = applyConfig(cfg); err != nil {
		log.Fatalf("error while applying config: %s", err)
	}
	log.Infof("Loading config %q: successful", *configFile)

	c := make(chan os.Signal)
	signal.Notify(c, syscall.SIGHUP)
	go func() {
		for {
			switch <-c {
			case syscall.SIGHUP:
				log.Infof("SIGHUP received. Going to reload config %s ...", *configFile)
				if err := reloadConfig(); err != nil {
					log.Errorf("error while reloading config: %s", err)
					continue
				}
				log.Infof("Reloading config %s: successful", *configFile)
			}
		}
	}()

	server := cfg.Server
	if len(server.HTTP.ListenAddr) == 0 && len(server.HTTPS.ListenAddr) == 0 {
		panic("BUG: broken config validation - `listen_addr` is not configured")
	}

	if server.HTTP.ForceAutocertHandler {
		autocertManager = newAutocertManager(server.HTTPS.Autocert)
	}
	if len(server.HTTPS.ListenAddr) != 0 {
		go serveTLS(server.HTTPS)
	}
	if len(server.HTTP.ListenAddr) != 0 {
		go serve(server.HTTP)
	}

	select {}
}

var autocertManager *autocert.Manager

func newAutocertManager(cfg config.Autocert) *autocert.Manager {
	if len(cfg.CacheDir) > 0 {
		if err := os.MkdirAll(cfg.CacheDir, 0700); err != nil {
			log.Fatalf("error while creating folder %q: %s", cfg.CacheDir, err)
		}
	}
	var hp autocert.HostPolicy
	if len(cfg.AllowedHosts) != 0 {
		allowedHosts := make(map[string]struct{}, len(cfg.AllowedHosts))
		for _, v := range cfg.AllowedHosts {
			allowedHosts[v] = struct{}{}
		}
		hp = func(_ context.Context, host string) error {
			if _, ok := allowedHosts[host]; ok {
				return nil
			}
			return fmt.Errorf("host %q doesn't match `host_policy` configuration", host)
		}
	}
	return &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(cfg.CacheDir),
		HostPolicy: hp,
	}
}

func newListener(listenAddr string) net.Listener {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("cannot listen for %q: %s", listenAddr, err)
	}
	return ln
}

func serveTLS(cfg config.HTTPS) {
	ln := newListener(cfg.ListenAddr)
	h := http.HandlerFunc(serveHTTP)
	tlsCfg := newTLSConfig(cfg)
	tln := tls.NewListener(ln, tlsCfg)
	log.Infof("Serving https on %q", cfg.ListenAddr)
	if err := listenAndServe(tln, h, cfg.TimeoutCfg); err != nil {
		log.Fatalf("TLS server error on %q: %s", cfg.ListenAddr, err)
	}
}

func serve(cfg config.HTTP) {
	var h http.Handler
	ln := newListener(cfg.ListenAddr)
	h = http.HandlerFunc(serveHTTP)
	if cfg.ForceAutocertHandler {
		if autocertManager == nil {
			panic("BUG: autocertManager is not inited")
		}
		addr := ln.Addr().String()
		parts := strings.Split(addr, ":")
		if parts[len(parts)-1] != "80" {
			log.Fatalf("`letsencrypt` specification requires http server to listen on :80 port to satisfy http-01 challenge. " +
				"Otherwise, certificates will be impossible to generate")
		}
		h = autocertManager.HTTPHandler(h)
	}
	log.Infof("Serving http on %q", cfg.ListenAddr)
	if err := listenAndServe(ln, h, cfg.TimeoutCfg); err != nil {
		log.Fatalf("HTTP server error on %q: %s", cfg.ListenAddr, err)
	}
}

func newTLSConfig(cfg config.HTTPS) *tls.Config {
	tlsCfg := tls.Config{
		PreferServerCipherSuites: true,
		CurvePreferences: []tls.CurveID{
			tls.CurveP256,
			tls.X25519,
		},
	}
	if len(cfg.KeyFile) > 0 && len(cfg.CertFile) > 0 {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			log.Fatalf("cannot load cert for `https.cert_file`=%q, `https.key_file`=%q: %s",
				cfg.CertFile, cfg.KeyFile, err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	} else {
		if autocertManager == nil {
			panic("BUG: autocertManager is not inited")
		}
		tlsCfg.GetCertificate = autocertManager.GetCertificate
	}
	return &tlsCfg
}

func listenAndServe(ln net.Listener, h http.Handler, cfg config.TimeoutCfg) error {
	s := &http.Server{
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
		Handler:      h,
		ReadTimeout:  time.Duration(cfg.ReadTimeout),
		WriteTimeout: time.Duration(cfg.WriteTimeout),
		IdleTimeout:  time.Duration(cfg.IdleTimeout),

		// Suppress error logging from the server, since chproxy
		// must handle all these errors in the code.
		ErrorLog: log.NilLogger,
	}
	return s.Serve(ln)
}

var promHandler = promhttp.Handler()

func serveHTTP(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
		// Only GET and POST methods are supported.
	case http.MethodOptions:
		// This is required for CORS shit :)
		rw.Header().Set("Allow", "GET,POST")
		return
	default:
		err := fmt.Errorf("%q: unsupported method %q", r.RemoteAddr, r.Method)
		rw.Header().Set("Connection", "close")
		respondWith(rw, err, http.StatusMethodNotAllowed)
		return
	}

	switch r.URL.Path {
	case "/favicon.ico":
	case "/metrics":
		an := allowedNetworksMetrics.Load().(*config.Networks)
		if !an.Contains(r.RemoteAddr) {
			err := fmt.Errorf("connections to /metrics are not allowed from %s", r.RemoteAddr)
			rw.Header().Set("Connection", "close")
			respondWith(rw, err, http.StatusForbidden)
			return
		}
		proxy.refreshCacheMetrics()
		promHandler.ServeHTTP(rw, r)
	case "/", "/query":
		var err error
		var an *config.Networks
		if r.TLS != nil {
			an = allowedNetworksHTTPS.Load().(*config.Networks)
			err = fmt.Errorf("https connections are not allowed from %s", r.RemoteAddr)
		} else {
			an = allowedNetworksHTTP.Load().(*config.Networks)
			err = fmt.Errorf("http connections are not allowed from %s", r.RemoteAddr)
		}
		if !an.Contains(r.RemoteAddr) {
			rw.Header().Set("Connection", "close")
			respondWith(rw, err, http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(rw, r)
	default:
		badRequest.Inc()
		err := fmt.Errorf("%q: unsupported path: %q", r.RemoteAddr, r.URL.Path)
		rw.Header().Set("Connection", "close")
		respondWith(rw, err, http.StatusBadRequest)
	}
}

func loadConfig() (*config.Config, error) {
	if *configFile == "" {
		log.Fatalf("Missing -config flag")
	}
	cfg, err := config.LoadFile(*configFile)
	if err != nil {
		configSuccess.Set(0)
		return nil, fmt.Errorf("can't load config %q: %s", *configFile, err)
	}
	configSuccess.Set(1)
	configSuccessTime.Set(float64(time.Now().Unix()))
	return cfg, nil
}

func applyConfig(cfg *config.Config) error {
	if err := proxy.applyConfig(cfg); err != nil {
		return err
	}
	allowedNetworksHTTP.Store(&cfg.Server.HTTP.AllowedNetworks)
	allowedNetworksHTTPS.Store(&cfg.Server.HTTPS.AllowedNetworks)
	allowedNetworksMetrics.Store(&cfg.Server.Metrics.AllowedNetworks)
	log.SetDebug(cfg.LogDebug)
	log.Infof("Loaded config:\n%s", cfg)

	return nil
}

func reloadConfig() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	return applyConfig(cfg)
}

var (
	buildTag      = "unknown"
	buildRevision = "unknown"
	buildTime     = "unknown"
)

func versionString() string {
	ver := buildTag
	if len(ver) == 0 {
		ver = "unknown"
	}
	return fmt.Sprintf("chproxy ver. %s, rev. %s, built at %s", ver, buildRevision, buildTime)
}
