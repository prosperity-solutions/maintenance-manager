// Package authz serves Envoy's gRPC ext_authz Check() (docs/HANDOVER.md §6).
//
// The hot path does no I/O, takes no locks and logs nothing per request. Every
// answer is a precomputed, shared *CheckResponse: gRPC only reads it.
package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
)

// Addr is the authz listen address. Hardcoded on purpose (§11): the
// SecurityPolicy and the Service both name port 9001.
const Addr = ":9001"

// gracefulStopTimeout caps GracefulStop on shutdown (§11).
const gracefulStopTimeout = 5 * time.Second

// okResponse lets a request through, untouched.
var okResponse = &authv3.CheckResponse{
	Status:       &status.Status{Code: int32(codes.OK)},
	HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{}},
}

// Server implements envoy.service.auth.v3.Authorization.
type Server struct {
	authv3.UnimplementedAuthorizationServer
	Addr string
	Log  *slog.Logger
}

// Check answers whether a request may pass. Until windows exist (M3) every
// request passes.
func (s *Server) Check(context.Context, *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	return okResponse, nil
}

// NeedLeaderElection reports false: every replica answers Check().
func (s *Server) NeedLeaderElection() bool { return false }

// Start serves until ctx is done, then stops gracefully within
// gracefulStopTimeout. It implements controller-runtime's manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	addr := s.Addr
	if addr == "" {
		addr = Addr
	}
	var lc net.ListenConfig
	lis, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("authz: listen on %s: %w", addr, err)
	}
	srv := grpc.NewServer(
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	authv3.RegisterAuthorizationServer(srv, s)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(lis) }()
	s.Log.Info("authz serving", "addr", lis.Addr().String())

	select {
	case err := <-errc:
		return fmt.Errorf("authz: serve: %w", err)
	case <-ctx.Done():
	}
	stopped := make(chan struct{})
	go func() { srv.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(gracefulStopTimeout):
		srv.Stop()
	}
	if err := <-errc; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("authz: serve: %w", err)
	}
	return nil
}
