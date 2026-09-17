package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeDrainsHandlers(t *testing.T) {
	listening := make(chan string, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			t.Error("graceful shutdown cancelled active request")
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	server.Addr = "127.0.0.1:0"
	server.BaseContext = func(listener net.Listener) context.Context {
		listening <- listener.Addr().String()
		return context.Background()
	}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	go func() { finished <- serve(ctx, server, time.Second, logger) }()
	var address string
	select {
	case address = <-listening:
	case err := <-finished:
		t.Fatalf("server did not start: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("server startup timed out")
	}
	response := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		result, err := client.Get("http://" + address)
		if err == nil {
			result.Body.Close()
		}
		response <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-finished:
		t.Fatalf("server returned before handler drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not complete")
	}
}

func TestForcedShutdownClosesConnections(t *testing.T) {
	listening := make(chan string, 1)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
		<-release // Shutdown must remain bounded even if a handler is stuck.
	})}
	server.Addr = "127.0.0.1:0"
	server.BaseContext = func(listener net.Listener) context.Context {
		listening <- listener.Addr().String()
		return context.Background()
	}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	go func() { finished <- serve(ctx, server, 20*time.Millisecond, logger) }()
	var address string
	select {
	case address = <-listening:
	case err := <-finished:
		t.Fatalf("server did not start: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("server startup timed out")
	}
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		client := &http.Client{Timeout: 2 * time.Second}
		response, err := client.Get("http://" + address)
		if err == nil {
			response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("forced shutdown did not cancel request")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("forced shutdown should report deadline: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not finish")
	}
	<-clientDone
}

func TestServeReportsBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	server := &http.Server{Addr: occupied.Addr().String()}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = serve(ctx, server, time.Second, logger)
	var networkError *net.OpError
	if !errors.As(err, &networkError) || networkError.Op != "listen" {
		t.Fatalf("expected listen error, got %v", err)
	}
}
