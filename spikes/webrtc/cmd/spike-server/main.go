package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/windshare/windshare/spikes/webrtc/internal/harness"
)

const shutdownTimeout = 5 * time.Second

func main() {
	address := os.Getenv("WINDSHARE_SPIKE_ADDR")
	if address == "" {
		address = harness.DefaultAddress
	}
	spike, err := harness.New()
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := spike.Close(); err != nil {
			log.Printf("close spike harness: %v", err)
		}
	}()

	server := &http.Server{
		Addr:              address,
		Handler:           spike.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stopped
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("shut down HTTP server: %v", err)
		}
	}()

	log.Printf("WindShare WebRTC spike listening on http://%s", address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(fmt.Errorf("serve spike harness: %w", err))
	}
}
