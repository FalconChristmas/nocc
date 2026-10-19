package client

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VKCOM/nocc/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func newTestDaemonWithRemote() (*Daemon, *RemoteConnection) {
	remote := &RemoteConnection{remoteHostPort: "host0:43210", remoteHost: "host0", grpcClient: &GRPCClient{}}
	daemon := &Daemon{
		remoteConnections: []*RemoteConnection{remote},
		activeInvocations: make(map[uint32]*Invocation),
	}
	return daemon, remote
}

// A stream of a connection that a reconnect already replaced still fails on its way out;
// that must not take down the fresh connection to the same host.
func TestStaleConnectionDoesNotMarkItsSuccessorUnavailable(t *testing.T) {
	daemon, remote := newTestDaemonWithRemote()
	replacedGrpcClient := &GRPCClient{}

	daemon.OnRemoteBecameUnavailable(replacedGrpcClient, errors.New("stream of a replaced connection"))
	if remote.isUnavailable.Load() {
		t.Fatal("a replaced connection marked the current one to the same host unavailable")
	}

	daemon.OnRemoteBecameUnavailable(remote.grpcClient, errors.New("stream broke"))
	if !remote.isUnavailable.Load() {
		t.Fatal("the current connection wasn't marked unavailable by its own stream")
	}
}

// A compilation in flight on a remote that went down must fail right away (to be retried),
// not wait for forceInterruptTimeout.
func TestRemoteGoingDownInterruptsItsInvocations(t *testing.T) {
	daemon, remote := newTestDaemonWithRemote()
	other := &RemoteConnection{remoteHost: "host1", grpcClient: &GRPCClient{}}

	onRemote := &Invocation{sessionID: 1, remote: remote, summary: MakeInvocationSummary()}
	onOther := &Invocation{sessionID: 2, remote: other, summary: MakeInvocationSummary()}
	for _, invocation := range []*Invocation{onRemote, onOther} {
		invocation.wgRecv.Add(1)
		daemon.activeInvocations[invocation.sessionID] = invocation
	}

	daemon.OnRemoteBecameUnavailable(remote.grpcClient, errors.New("stream broke"))

	released := make(chan struct{})
	go func() { onRemote.wgRecv.Wait(); close(released) }()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("an invocation on the remote that went down is still waiting for its .o")
	}
	if onRemote.err == nil {
		t.Error("the interrupted invocation has no error, so it would be taken as compiled")
	}
	if onOther.err != nil {
		t.Error("an invocation on another remote was interrupted too")
	}
}

// ForceInterrupt drains pending uploads while the upload stream may still be finishing them;
// together they must never release wgUpload more times than it was added (that panics).
func TestUploadsFinishingDuringInterruptDontPanic(t *testing.T) {
	for i := 0; i < 1000; i++ {
		invocation := &Invocation{summary: MakeInvocationSummary()}
		invocation.waitUploads = 5
		invocation.wgUpload.Add(5)
		invocation.wgRecv.Add(1) // as CompileCppRemotely does; ForceInterrupt releases it too

		var wg sync.WaitGroup
		wg.Add(6)
		for j := 0; j < 5; j++ {
			go func() { defer wg.Done(); invocation.DoneUploadFile(nil) }()
		}
		go func() { defer wg.Done(); invocation.ForceInterrupt(errors.New("interrupted")) }()
		wg.Wait()
		invocation.wgUpload.Wait()
	}
}

func TestRemoteRetryDelay(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for attempt, delay := range want {
		if got := remoteRetryDelay(attempt); got != delay {
			t.Errorf("attempt %d: got %v, want %v", attempt, got, delay)
		}
	}
}

type fakeRecvStream struct {
	grpc.ClientStream
	replies chan *pb.RecvCompiledObjChunkReply
}

func (f *fakeRecvStream) Recv() (*pb.RecvCompiledObjChunkReply, error) {
	return <-f.replies, nil
}

// A server that predates its own check may still report a killed compiler as exit code -1;
// the client must take that as a remote failure (and retry), not as a compile error to show.
func TestKilledRemoteCompilerIsARemoteFailure(t *testing.T) {
	daemon, _ := newTestDaemonWithRemote()
	daemon.quitChan = make(chan int)
	invocation := &Invocation{sessionID: 5, summary: MakeInvocationSummary()}
	invocation.wgRecv.Add(1)
	daemon.activeInvocations[invocation.sessionID] = invocation

	stream := &fakeRecvStream{replies: make(chan *pb.RecvCompiledObjChunkReply, 1)}
	stream.replies <- &pb.RecvCompiledObjChunkReply{SessionID: 5, CxxExitCode: -1, CxxStderr: []byte("signal: terminated")}
	go (&FilesReceiving{daemon: daemon}).monitorRemoteStreamForObjReceiving(stream, func() {})

	invocation.wgRecv.Wait()
	if invocation.err == nil {
		t.Fatalf("exit code %d was taken as a compile result, so it would never be retried", invocation.cxxExitCode)
	}
}

// brokenRecvStream is a .o stream that has already broken, with the trailer the server managed to send, if any.
type brokenRecvStream struct {
	grpc.ClientStream
	err     error
	trailer metadata.MD
}

func (f *brokenRecvStream) Recv() (*pb.RecvCompiledObjChunkReply, error) { return nil, f.err }
func (f *brokenRecvStream) Trailer() metadata.MD                         { return f.trailer }

// idleRecvStream is a healthy .o stream with nothing to deliver until the daemon quits.
type idleRecvStream struct {
	grpc.ClientStream
	quit chan int
}

func (f *idleRecvStream) Recv() (*pb.RecvCompiledObjChunkReply, error) {
	<-f.quit
	return nil, errors.New("daemon quit")
}

// reconnectingServer is a server that a broken .o stream is opened to again, successfully.
type reconnectingServer struct {
	pb.CompilationServiceClient
	quit          chan int
	streamsOpened atomic.Int32
}

func (f *reconnectingServer) RecvCompiledObjStream(context.Context, *pb.OpenReceiveStreamRequest, ...grpc.CallOption) (pb.CompilationService_RecvCompiledObjStreamClient, error) {
	f.streamsOpened.Add(1)
	return &idleRecvStream{quit: f.quit}, nil
}

// waitingForObj registers an invocation on remote that has finished uploading and waits for its .o.
func waitingForObj(daemon *Daemon, sessionID uint32, remote *RemoteConnection) *Invocation {
	invocation := &Invocation{sessionID: sessionID, remote: remote, summary: MakeInvocationSummary()}
	invocation.wgRecv.Add(1)
	daemon.activeInvocations[sessionID] = invocation
	return invocation
}

func isReleased(invocation *Invocation, within time.Duration) bool {
	released := make(chan struct{})
	go func() { invocation.wgRecv.Wait(); close(released) }()
	select {
	case <-released:
		return true
	case <-time.After(within):
		return false
	}
}

func newDaemonWithReconnectingRemote(t *testing.T) (*Daemon, *RemoteConnection, *reconnectingServer) {
	daemon, remote := newTestDaemonWithRemote()
	daemon.quitChan = make(chan int)
	t.Cleanup(func() { close(daemon.quitChan) })
	server := &reconnectingServer{quit: daemon.quitChan}
	remote.grpcClient = &GRPCClient{callContext: context.Background(), pb: server}
	return daemon, remote, server
}

// A .o stream that broke under the transport, with no word from the server, may have taken .o files with it:
// the server pushed them into the stream and takes them as delivered. Every compilation waiting on that server
// must fail right away (to be retried), even though the stream reconnects fine. Before, they waited for
// forceInterruptTimeout -- 8 minutes of a build standing still, then a retry.
func TestBrokenObjStreamFailsTheInvocationsWaitingOnIt(t *testing.T) {
	daemon, remote, server := newDaemonWithReconnectingRemote(t)
	other := &RemoteConnection{remoteHost: "host1", grpcClient: &GRPCClient{}}
	onRemote := waitingForObj(daemon, 1, remote)
	onOther := waitingForObj(daemon, 2, other)

	broken := &brokenRecvStream{err: status.Error(codes.Unavailable, "transport is closing")}
	go (&FilesReceiving{daemon: daemon, grpcClient: remote.grpcClient}).monitorRemoteStreamForObjReceiving(broken, func() {})

	if !isReleased(onRemote, time.Second) {
		t.Fatal("an invocation whose .o may have been lost with the stream is still waiting for it")
	}
	if onRemote.err == nil {
		t.Error("the interrupted invocation has no error, so it would be taken as compiled")
	}
	if onOther.err != nil || isReleased(onOther, 0) {
		t.Error("an invocation on another remote was interrupted too")
	}
	if remote.isUnavailable.Load() {
		t.Error("the remote was marked unavailable, though its stream reconnected")
	}
	deadline := time.Now().Add(time.Second)
	for server.streamsOpened.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if server.streamsOpened.Load() != 1 {
		t.Errorf("the .o stream was opened again %d times, want 1", server.streamsOpened.Load())
	}
	if remote.isUnavailable.Load() {
		t.Error("the remote was marked unavailable after its stream reconnected")
	}
}

// When the server ends the stream itself, the trailer names the one session it failed to send,
// and arrives after everything sent before it: the other compilations on that server are fine.
func TestObjStreamEndedByTheServerFailsOnlyItsSession(t *testing.T) {
	daemon, remote, _ := newDaemonWithReconnectingRemote(t)
	failed := waitingForObj(daemon, 1, remote)
	stillCompiling := waitingForObj(daemon, 2, remote)

	broken := &brokenRecvStream{err: status.Error(codes.Unknown, "the C++ compiler was killed"), trailer: metadata.Pairs("sessionID", "1")}
	go (&FilesReceiving{daemon: daemon, grpcClient: remote.grpcClient}).monitorRemoteStreamForObjReceiving(broken, func() {})

	if !isReleased(failed, time.Second) || failed.err == nil {
		t.Fatal("the session named in the trailer wasn't failed")
	}
	if isReleased(stillCompiling, 300*time.Millisecond) {
		t.Error("a session the server will still deliver was failed too, so it gets compiled twice")
	}
}
