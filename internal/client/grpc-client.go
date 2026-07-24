package client

import (
	"context"
	"time"

	"github.com/VKCOM/nocc/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

type GRPCClient struct {
	remoteHostPort string
	connection     *grpc.ClientConn
	callContext    context.Context
	cancelFunc     context.CancelFunc
	pb             pb.CompilationServiceClient
}

func MakeGRPCClient(remoteHostPort string) (*GRPCClient, error) {
	// this connection is non-blocking: it's created immediately
	// if the remote is not available, it will fail on request
	connection, err := grpc.Dial(
		remoteHostPort,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(),
		// Keep the connection warm so a router/NAT/conntrack table between client and
		// server doesn't silently evict an idle stream (which surfaces later as
		// "connection reset by peer" and drops the whole build to local compilation).
		// PermitWithoutStream is the important part: on slow clients the connection sits
		// idle *between* files, with no active RPC, which is exactly when it gets dropped.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                20 * time.Second, // ping after 20s of inactivity
			Timeout:             10 * time.Second, // wait 10s for the ping ack
			PermitWithoutStream: true,             // ping even with no active RPC
		}),
	)
	if err != nil {
		return nil, err
	}

	ctx, cancelFunc := context.WithCancel(context.Background())
	return &GRPCClient{
		remoteHostPort: remoteHostPort,
		connection:     connection,
		callContext:    ctx,
		cancelFunc:     cancelFunc,
		pb:             pb.NewCompilationServiceClient(connection),
	}, nil
}

func (grpcClient *GRPCClient) Clear() {
	if grpcClient.connection != nil {
		grpcClient.cancelFunc()
		_ = grpcClient.connection.Close()

		grpcClient.connection = nil
		grpcClient.callContext = nil
		grpcClient.cancelFunc = nil
		grpcClient.pb = nil
	}
}
