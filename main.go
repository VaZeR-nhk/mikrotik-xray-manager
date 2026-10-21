// Command xray-manager serves a web UI for managing Xray-core proxy configs.
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"xray-manager/internal/auth"
	"xray-manager/internal/config"
	"xray-manager/internal/handlers"
	"xray-manager/internal/storage"
	"xray-manager/internal/xray"
)

//go:embed all:web/static
var staticFiles embed.FS

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[xray-manager] ")

	cfg := config.Load()

	store, err := storage.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}

	authMgr := auth.New(cfg.Password, cfg.SessionSecret)
	xrayMgr := xray.NewManager(cfg.XrayBinary, cfg.XrayConfigPath, cfg.InboundSocks, cfg.InboundHTTP)

	// Restore persisted DNS settings so generated configs match the UI.
	xrayMgr.SetDNS(store.Settings().DNSServers)

	// Restore persisted routing rules so generated configs match the UI.
	xrayMgr.SetRoutingRules(store.RoutingRules())

	// Restore last-known active proxy into the manager for status display.
	if active := store.Active().ProxyID; active != "" {
		xrayMgr.SetActiveID(active)
	}
	if !xrayMgr.BinaryAvailable() {
		log.Printf("WARNING: xray binary not found at %q — process controls will be disabled until it exists.", cfg.XrayBinary)
	}

	// Auto-start xray with the active proxy so clients (e.g. the hotspot) get
	// their connection back after a reboot or container restart. The config is
	// regenerated first so it always matches the current code and settings.
	if active := store.Active().ProxyID; active != "" && xrayMgr.BinaryAvailable() {
		if p, err := store.Proxy(active); err != nil {
			log.Printf("autostart: active proxy %s not found: %v", active, err)
		} else if err := xrayMgr.WriteConfig(&p); err != nil {
			log.Printf("autostart: write config: %v", err)
		} else if err := xrayMgr.Start(); err != nil {
			log.Printf("autostart: start xray: %v", err)
		} else {
			log.Printf("autostart: xray started with active proxy %q", p.Name)
		}
	}

	staticFS, err := fs.Sub(staticFiles, "web/static")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	app := handlers.New(cfg, store, authMgr, xrayMgr, staticFS)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown.
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop
		log.Println("shutting down…")

		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
		if err := xrayMgr.Stop(); err != nil {
			log.Printf("xray stop: %v", err)
		}
	}()

	log.Printf("listening on http://0.0.0.0:%d  (data: %s)", cfg.Port, cfg.DataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
	log.Println("bye")
}
