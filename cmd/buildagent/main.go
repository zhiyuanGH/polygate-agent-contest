package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"polygate/internal/buildagent"
)

func parseSigningPublicKey(data []byte) (ed25519.PublicKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || len(rest) != 0 {
		return nil, errors.New("signing public key must be PKIX PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	key, ok := parsed.(ed25519.PublicKey)
	if err != nil || !ok || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 signing public key")
	}
	return key, nil
}

func listenAddress(value string) (string, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil || port == "" || port == "0" {
		return "", errors.New("Agent listen address requires a fixed IP and port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() {
		return "", errors.New("Agent must bind a specific host interface")
	}
	return net.JoinHostPort(host, port), nil
}

func main() {
	if os.Geteuid() != 0 {
		log.Fatal("Build Agent requires the dedicated root-managed host service")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workRoot := os.Getenv("POLYGATE_BUILD_WORK_ROOT")
	lock, err := buildagent.AcquireHostLock(workRoot)
	if err != nil {
		log.Fatal("Build Agent host lock failed: ", err)
	}
	defer lock.Close()
	commands := buildagent.ExecCommandRunner{}
	backend := os.Getenv("POLYGATE_BUILD_BACKEND")
	if backend == "" {
		backend = "vm"
	}
	var runner buildagent.Runner
	var validate func() error
	var preflightArgs []string
	var preflightExpected string
	switch backend {
	case "vm":
		vm := buildagent.VMRunner{Cleaner: buildagent.LibvirtCleaner{
			VirshPath: os.Getenv("POLYGATE_BUILD_VIRSH_PATH"), WorkRoot: workRoot,
			Commands: commands}, HelperPath: os.Getenv("POLYGATE_BUILD_HELPER_PATH"),
			HelperSHA256:    os.Getenv("POLYGATE_BUILD_HELPER_SHA256"),
			BaseImagePath:   os.Getenv("POLYGATE_BUILD_BASE_IMAGE_PATH"),
			BaseImageSHA256: os.Getenv("POLYGATE_BUILD_BASE_IMAGE_SHA256"),
			TrustedOwnerUID: 0, Commands: commands}
		runner, validate = vm, vm.Validate
		preflightArgs = []string{"--preflight", "--work-root", workRoot, "--base-image", vm.BaseImagePath}
		preflightExpected = "POLYGATE_VM_PREFLIGHT_OK\n"
	case "trusted-docker":
		trusted := buildagent.TrustedDockerRunner{Cleaner: buildagent.DockerCleaner{
			DockerPath: "/usr/bin/docker", WorkRoot: workRoot, Commands: commands},
			HelperPath:      os.Getenv("POLYGATE_BUILD_HELPER_PATH"),
			HelperSHA256:    os.Getenv("POLYGATE_BUILD_HELPER_SHA256"),
			TrustedOwnerUID: 0, Commands: commands}
		runner, validate = trusted, trusted.Validate
		preflightArgs = []string{"--preflight", "--work-root", workRoot}
		preflightExpected = "POLYGATE_TRUSTED_PREFLIGHT_OK\n"
	default:
		log.Fatal("unsupported Build Agent backend")
	}
	manager := buildagent.NewManager(runner)
	recoveryCtx, cancel := context.WithTimeout(ctx, buildagent.CleanupTimeout)
	err = manager.Recover(recoveryCtx)
	cancel()
	if err != nil {
		log.Fatal("Build Agent startup recovery failed: ", err)
	}
	if err = validate(); err != nil {
		log.Fatal("Build Agent immutable helper configuration is invalid: ", err)
	}
	preflightCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	preflight, preflightErr := commands.Run(preflightCtx, os.Getenv("POLYGATE_BUILD_HELPER_PATH"), preflightArgs...)
	cancel()
	if preflightErr != nil || strings.TrimSpace(preflight) != strings.TrimSpace(preflightExpected) {
		log.Fatal("Build Agent host preflight did not pass")
	}
	serverCert := readOwnedSecret("POLYGATE_BUILD_SERVER_CERT_PATH")
	serverKey := readOwnedSecret("POLYGATE_BUILD_SERVER_KEY_PATH")
	workerCA := readOwnedSecret("POLYGATE_BUILD_WORKER_CA_PATH")
	publicPEM := readOwnedSecret("POLYGATE_BUILD_SIGNING_PUBLIC_KEY_PATH")
	publicKey, err := parseSigningPublicKey(publicPEM)
	if err != nil {
		log.Fatal("Build Agent task-signing key is invalid")
	}
	tlsConfig, err := buildagent.ServerTLSConfig(serverCert, serverKey, workerCA)
	if err != nil {
		log.Fatal("Build Agent mTLS configuration is invalid")
	}
	handler, err := buildagent.NewHandler(manager, publicKey,
		os.Getenv("POLYGATE_BUILD_STAGING_ROOT"))
	if err != nil {
		log.Fatal("Build Agent staging or grant ledger is invalid: ", err)
	}
	address, err := listenAddress(os.Getenv("POLYGATE_BUILD_AGENT_LISTEN"))
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal("Build Agent listen failed: ", err)
	}
	server := &http.Server{Handler: handler, TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 18 * time.Minute,
		WriteTimeout: 18 * time.Minute, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 4096}
	done := make(chan error, 1)
	go func() { done <- server.Serve(tls.NewListener(listener, tlsConfig)) }()
	log.Print("PolyGate Build Agent ready after ", backend, " recovery and host preflight")
	select {
	case err = <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Print("Build Agent listener stopped: ", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
		if err = server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			log.Print("Build Agent shutdown exceeded its grace period: ", err)
		}
		cancel()
		<-done
	}
}
