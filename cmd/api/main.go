package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"polygate/internal/applogs"
	"polygate/internal/auth"
	"polygate/internal/harbor"
	"polygate/internal/keycloakadmin"
	"polygate/internal/kubeobjects"
	"polygate/internal/postgres"
	"polygate/internal/registry"
	"polygate/internal/secretcatalog"
	"polygate/internal/server"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--prestop" {
		// Keep accepting connections while terminating Endpoints propagate.
		time.Sleep(20 * time.Second)
		return
	}
	var handler http.Handler
	addr := "127.0.0.1:8080"
	if os.Getenv("POLYGATE_DEV_MODE") == "1" {
		if v := os.Getenv("POLYGATE_DEV_LISTEN"); v != "" {
			if v != "127.0.0.1:8080" && v != "localhost:8080" {
				log.Fatal("Development API may only listen on localhost:8080")
			}
			addr = v
		}
		handler = server.NewDevHandler(server.NewMemoryStore())
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		store, err := postgres.Open(ctx, postgres.DatabaseURLFromEnvironment())
		if err != nil {
			log.Fatal("Database connection failed; run migrations and check POLYGATE_DATABASE_URL")
		}
		defer store.Close()
		identity, err := auth.New(ctx, store.Pool, auth.Config{
			IssuerURL: os.Getenv("POLYGATE_OIDC_ISSUER"), ClientID: os.Getenv("POLYGATE_OIDC_CLIENT_ID"),
			ClientSecret: os.Getenv("POLYGATE_OIDC_CLIENT_SECRET"), RedirectURL: os.Getenv("POLYGATE_OIDC_REDIRECT_URL"),
			BaseURL: os.Getenv("POLYGATE_BASE_URL"), CSRFKey: []byte(os.Getenv("POLYGATE_CSRF_KEY")),
		})
		if err != nil {
			log.Fatal("OIDC configuration or discovery failed: ", err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := store.Pool.Ping(ctx); err != nil {
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
		})
		mux.HandleFunc("GET /queuez", func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			healthy, err := store.SourceQueueHealthy(ctx)
			if err != nil || !healthy {
				http.Error(w, "source queue unhealthy", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("source queue healthy"))
		})
		mux.HandleFunc("GET /deployment-queuez", func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			healthy, err := store.DeploymentQueueHealthy(ctx)
			if err != nil || !healthy {
				http.Error(w, "deployment queue unhealthy", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("deployment queue healthy"))
		})
		mux.HandleFunc("GET /secret-queuez", func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			healthy, err := store.SecretMutationBacklogHealthy(ctx)
			if err != nil || !healthy {
				http.Error(w, "student secret queue unhealthy", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("student secret queue healthy"))
		})
		mux.Handle("/auth/", identity.Routes())
		mux.Handle("GET /auth/app", identity.AppAccessHandler(store, os.Getenv("POLYGATE_PRIVATE_APP_DOMAIN")))
		trustedDigest := ""
		trustedPort := 0
		if os.Getenv("POLYGATE_DEPLOY_ENABLED") == "1" {
			trustedDigest = os.Getenv("POLYGATE_TRUSTED_SAMPLE_DIGEST")
			if err := kubeobjects.ValidatePlatformDigest(trustedDigest); err != nil {
				log.Fatal("Trusted deployment image digest is invalid: ", err)
			}
			trustedPort = 5678 // The approved http-echo sample listens on this port.
		}
		var inspect func(context.Context, string) (registry.Image, error)
		if policyJSON := os.Getenv("POLYGATE_REGISTRY_POLICY_JSON"); policyJSON != "" {
			decoder := json.NewDecoder(strings.NewReader(policyJSON))
			decoder.DisallowUnknownFields()
			var policy registry.Policy
			if err := decoder.Decode(&policy); err != nil {
				log.Fatal("Registry policy is invalid: ", err)
			}
			if err := decoder.Decode(&struct{}{}); err != io.EOF {
				log.Fatal("Registry policy must contain exactly one JSON object")
			}
			resolver, err := registry.NewResolver(policy, nil)
			if err != nil {
				log.Fatal("Registry policy is invalid: ", err)
			}
			inspect = func(ctx context.Context, ref string) (registry.Image, error) {
				return resolver.Resolve(ctx, ref, nil)
			}
		}
		var apiHandler http.Handler
		buildEnabled := os.Getenv("POLYGATE_BUILD_SUBMISSION_ENABLED") == "1"
		buildRoot := ""
		if buildEnabled {
			buildRoot = os.Getenv("POLYGATE_SOURCE_ROOT")
			info, statErr := os.Lstat(buildRoot)
			if !filepath.IsAbs(buildRoot) || statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				log.Fatal("Build source root must be an existing real absolute directory")
			}
		}
		if os.Getenv("POLYGATE_APP_LOGS_ENABLED") == "1" {
			reader, err := applogs.NewInCluster()
			if err != nil {
				log.Fatal("Application log reader could not start: ", err)
			}
			if os.Getenv("POLYGATE_SECRET_ENABLED") == "1" {
				catalog, secretErr := secretcatalog.NewInCluster()
				if secretErr != nil {
					log.Fatal("Environment secret catalog could not start: ", secretErr)
				}
				go reconcileStudentSecrets(store, catalog)
				apiHandler = server.NewHandlerWithAppLogsAndSecrets(store, identity.Current,
					trustedDigest, trustedPort, inspect, os.Getenv("POLYGATE_IMAGE_DEPLOY_ENABLED") == "1",
					buildRoot, buildEnabled, reader, catalog)
			} else {
				apiHandler = server.NewHandlerWithAppLogs(store, identity.Current,
					trustedDigest, trustedPort, inspect, os.Getenv("POLYGATE_IMAGE_DEPLOY_ENABLED") == "1",
					buildRoot, buildEnabled, reader)
			}
		} else if buildEnabled {
			if os.Getenv("POLYGATE_SECRET_ENABLED") == "1" {
				log.Fatal("Environment secrets require the authenticated app log API")
			}
			apiHandler = server.NewHandlerWithBuildSubmission(store, identity.Current,
				trustedDigest, trustedPort, inspect, os.Getenv("POLYGATE_IMAGE_DEPLOY_ENABLED") == "1", buildRoot)
		} else {
			if os.Getenv("POLYGATE_SECRET_ENABLED") == "1" {
				log.Fatal("Environment secrets require the authenticated app log API")
			}
			apiHandler = server.NewHandlerWithImageDeployment(store, identity.Current,
				trustedDigest, trustedPort, inspect, os.Getenv("POLYGATE_IMAGE_DEPLOY_ENABLED") == "1")
		}
		if os.Getenv("POLYGATE_IMAGE_UPLOAD_ENABLED") == "1" {
			if buildRoot == "" {
				log.Fatal("Image upload requires the shared source root")
			}
			ca, err := os.ReadFile(os.Getenv("POLYGATE_HARBOR_CA_PATH"))
			if err != nil {
				log.Fatal("Image upload Harbor CA is unavailable")
			}
			client, err := harbor.New(harbor.Config{BaseURL: "https://" + harbor.RegistryHost,
				Username: "admin", Password: os.Getenv("POLYGATE_HARBOR_ADMIN_PASSWORD"), CACertPEM: ca})
			if err != nil {
				log.Fatal("Image upload Harbor connection is invalid")
			}
			apiHandler.(*server.Handler).SetImageUploader(client.ImportImageTar)
			apiHandler.(*server.Handler).SetImageReportReader(client.ReadVulnerabilityReport)
		}
		if privateDomain := os.Getenv("POLYGATE_PRIVATE_APP_DOMAIN"); privateDomain != "" {
			check, checkErr := server.NewHTTPPrivateAppRouteChecker(privateDomain)
			if checkErr != nil {
				log.Fatal("Private app route checker configuration is invalid: ", checkErr)
			}
			apiHandler.(*server.Handler).SetPrivateRouteChecker(check)
		}
		if os.Getenv("POLYGATE_ADMIN_PROVISION_ENABLED") == "1" {
			manager, err := keycloakadmin.New(os.Getenv("POLYGATE_KEYCLOAK_ADMIN_URL"),
				"polygate", "polygate-account-manager", os.Getenv("POLYGATE_KEYCLOAK_ADMIN_SECRET"))
			if err != nil {
				log.Fatal("Account manager configuration is invalid")
			}
			apiHandler = server.NewAccountAdministrationHandler(apiHandler, store, identity.Current, manager)
		}
		mux.Handle("/", identity.Protect(apiHandler))
		handler = mux
		if v := os.Getenv("POLYGATE_LISTEN"); v != "" {
			addr = v
		}
	}
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Minute}
	log.Printf("PolyGate API listening on %s", addr)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("API graceful shutdown: %v", err)
			_ = srv.Close()
		}
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-done
}

func reconcileStudentSecrets(store *postgres.Store, catalog *secretcatalog.Catalog) {
	probe := func(ctx context.Context, accountID, key, kind, operationID string) (bool, error) {
		return catalog.MutationApplied(ctx, accountID, key, kind, operationID)
	}
	check := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		resolved, err := store.ReconcileSecretMutations(ctx, probe)
		if err != nil {
			log.Printf("Student Secret mutation reconciliation failed: %v", err)
		} else if resolved > 0 {
			log.Printf("Student Secret mutations reconciled: %d", resolved)
		}
	}
	check()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		check()
	}
}
