package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"polygate/internal/appgateway"
	"polygate/internal/postgres"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--prestop" {
		time.Sleep(20 * time.Second)
		return
	}
	var suffixes []string
	var routes []appgateway.HostRoute
	var pathRoutes []appgateway.PathRoute
	var privateRoutes []appgateway.HostRoute
	var err error
	port := os.Getenv("POLYGATE_APP_PUBLIC_PORT")
	if raw := os.Getenv("POLYGATE_APP_HOST_ROUTES"); raw != "" {
		routes, err = appgateway.ParseHostRoutes(raw)
	} else {
		suffixes, err = appgateway.ParseSuffixes(os.Getenv("POLYGATE_APP_HOST_SUFFIXES"))
		if err == nil && port == "" {
			log.Fatal("POLYGATE_APP_PUBLIC_PORT is required")
		}
	}
	if err != nil {
		log.Fatal(err)
	}
	if raw := os.Getenv("POLYGATE_APP_PATH_ROUTES"); raw != "" {
		pathRoutes, err = appgateway.ParsePathRoutes(raw)
		if err != nil {
			log.Fatal(err)
		}
	}
	consoleOrigin := os.Getenv("POLYGATE_CONSOLE_ORIGIN")
	if raw := os.Getenv("POLYGATE_APP_PRIVATE_HOST_ROUTES"); raw != "" {
		privateRoutes, err = appgateway.ParseHostRoutes(raw)
		if err != nil {
			log.Fatal(err)
		}
		parsed, parseErr := url.Parse(consoleOrigin)
		if parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" ||
			parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			log.Fatal("private app routes require a valid HTTPS console origin")
		}
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}
	config.QPS, config.Burst = 10, 20
	config.Timeout = 10 * time.Second
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}
	dbContext, cancelDB := context.WithTimeout(context.Background(), 15*time.Second)
	store, err := postgres.Open(dbContext, postgres.DatabaseURLFromEnvironment())
	cancelDB()
	if err != nil {
		log.Fatal("app access database unavailable: ", err)
	}
	defer store.Close()
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	gateway := &appgateway.Gateway{
		Resolver:          appgateway.NewResolver(client),
		Access:            store,
		PrivateAccess:     store,
		Suffixes:          suffixes,
		PublicPort:        port,
		HostRoutes:        routes,
		PathRoutes:        pathRoutes,
		PrivateHostRoutes: privateRoutes,
		ConsoleOrigin:     consoleOrigin,
		Transport:         transport,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := store.Pool.Ping(ctx); err != nil {
			http.Error(w, "app access database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", gateway)
	server := &http.Server{
		Addr:              ":8443",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stop)
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-stop
		shutdown, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("app gateway graceful shutdown: %v", err)
			_ = server.Close()
		}
	}()
	log.Print("app gateway listening")
	if err := server.ListenAndServeTLS("/etc/polygate-app-tls/tls.crt", "/etc/polygate-app-tls/tls.key"); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-done
}
