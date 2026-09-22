package rpc

import (
	"context"
	"net"
	"os"
	"slices"

	"github.com/cirruslabs/tart-guest-agent/pkg/v1"
	"github.com/puzpuzpuz/xsync/v4"
	"google.golang.org/grpc"
)

type RPC struct {
	v1.UnimplementedAgentServer

	grpcServer  *grpc.Server
	listener    net.Listener
	execs       *xsync.Map[string, *os.Process]
	execWrapper []string
}

func New(listener net.Listener, execWrapper ...string) (*RPC, error) {
	if err := ValidateExecWrapper(execWrapper); err != nil {
		return nil, err
	}

	rpc := &RPC{
		grpcServer:  grpc.NewServer(),
		listener:    listener,
		execs:       xsync.NewMap[string, *os.Process](),
		execWrapper: slices.Clone(execWrapper),
	}

	v1.RegisterAgentServer(rpc.grpcServer, rpc)

	return rpc, nil
}

func (rpc *RPC) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()

		rpc.grpcServer.Stop()
	}()

	return rpc.grpcServer.Serve(rpc.listener)
}
