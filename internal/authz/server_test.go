package authz

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
)

func TestCheckPassesOverGRPC(t *testing.T) {
	// Grab a free port, then hand it to the server.
	var lc net.ListenConfig
	lis, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	ctx, cancel := context.WithCancel(t.Context())
	var shutdown atomic.Bool
	s := &Server{Addr: addr, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnShutdown: func() { shutdown.Store(true) }, Drain: 500 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	client := authv3.NewAuthorizationClient(conn)

	req := &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
			Method: "GET", Path: "/", Headers: map[string]string{"accept": "text/html"},
		}},
		Source: &authv3.AttributeContext_Peer{Address: &corev3.Address{}},
	}}
	var resp *authv3.CheckResponse
	deadline := time.Now().Add(5 * time.Second)
	for {
		cctx, ccancel := context.WithTimeout(t.Context(), time.Second)
		resp, err = client.Check(cctx, req)
		ccancel()
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if resp.GetStatus().GetCode() != int32(codes.OK) || resp.GetOkResponse() == nil {
		t.Fatalf("want OK, got %v", resp)
	}

	cancel()
	// During the drain Check() still answers.
	time.Sleep(100 * time.Millisecond)
	if !shutdown.Load() {
		t.Fatal("OnShutdown was not called when shutdown started")
	}
	cctx, ccancel := context.WithTimeout(t.Context(), time.Second)
	resp, err = client.Check(cctx, req)
	ccancel()
	if err != nil || resp.GetStatus().GetCode() != int32(codes.OK) {
		t.Fatalf("Check during the drain: %v, %v", resp, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v after cancel", err)
		}
		if !shutdown.Load() {
			t.Fatal("OnShutdown was not called before stopping")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop within 10s of cancel")
	}
}

func TestNeedLeaderElection(t *testing.T) {
	if (&Server{}).NeedLeaderElection() {
		t.Fatal("every replica must answer Check()")
	}
}

// The hot path budget (§6): at most 2 allocations beyond proto decoding.
func TestCheckAllocs(t *testing.T) {
	s := &Server{}
	req := &authv3.CheckRequest{}
	ctx := t.Context()
	if n := testing.AllocsPerRun(1000, func() { _, _ = s.Check(ctx, req) }); n > 2 {
		t.Fatalf("Check allocates %.0f times per call; the budget is 2", n)
	}
}
