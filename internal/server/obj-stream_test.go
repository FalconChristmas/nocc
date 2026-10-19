package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/VKCOM/nocc/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type fakeObjStream struct {
	grpc.ServerStream
	ctx      context.Context // nil means a stream that never ends
	failSend func()          // if set, called by Send instead of sending: the transport dying under it
	mu       sync.Mutex
	sent     []*pb.RecvCompiledObjChunkReply
	trailer  metadata.MD
}

func (f *fakeObjStream) Context() context.Context {
	if f.ctx == nil {
		return context.Background()
	}
	return f.ctx
}

// Send fails once the stream's context is done, as a grpc stream does.
func (f *fakeObjStream) Send(reply *pb.RecvCompiledObjChunkReply) error {
	if f.failSend != nil {
		f.failSend()
	}
	if err := f.Context().Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, reply)
	return nil
}

func (f *fakeObjStream) sentSessionIDs() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]uint32, 0, len(f.sent))
	for _, reply := range f.sent {
		ids = append(ids, reply.SessionID)
	}
	return ids
}

func (f *fakeObjStream) SetTrailer(md metadata.MD) {
	f.trailer = md
}

func newTestServerWithClient(t *testing.T) (*NoccServer, *Client) {
	t.Helper()
	storage, _ := MakeClientsStorage(t.TempDir(), time.Minute)
	client, err := storage.OnClientConnected("c1", false)
	if err != nil {
		t.Fatal(err)
	}
	return &NoccServer{ActiveClients: storage, Stats: &Statsd{}}, client
}

// A compiler killed under the server (the server stopping, the OOM killer) is not a compile error
// in the client's source; it must reach the client as a failure it retries.
func TestKilledCompilerIsNotReportedAsCompileError(t *testing.T) {
	s, client := newTestServerWithClient(t)
	client.chanReadySessions <- &Session{sessionID: 7, cxxExitCode: 1, cxxStderr: []byte("g++: fatal error: Killed signal terminated program cc1plus\ncompilation terminated.")}

	stream := &fakeObjStream{}
	done := make(chan error)
	go func() { done <- s.RecvCompiledObjStream(&pb.OpenReceiveStreamRequest{ClientID: "c1"}, stream) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second): // the stream went on waiting for more sessions; end it to look at what was sent
		s.ActiveClients.DeleteClient(client)
		err = <-done
	}

	if len(stream.sent) != 0 {
		t.Fatalf("sent a reply with exit code %d, which the client takes for a compile error", stream.sent[0].CxxExitCode)
	}
	if err == nil {
		t.Fatal("the stream didn't fail, so the client never learns this session failed")
	}
	if got := stream.trailer.Get("sessionID"); len(got) != 1 || got[0] != "7" {
		t.Errorf("trailer sessionID = %v, want [7]: without it, the client can't tell which invocation failed", got)
	}
}

// A real compile error still goes through as one.
func TestCompileErrorIsStillReported(t *testing.T) {
	s, client := newTestServerWithClient(t)
	client.chanReadySessions <- &Session{sessionID: 8, cxxExitCode: 1, cxxStderr: []byte("error: expected ';'")}

	stream := &fakeObjStream{}
	done := make(chan error)
	go func() { done <- s.RecvCompiledObjStream(&pb.OpenReceiveStreamRequest{ClientID: "c1"}, stream) }()
	time.Sleep(100 * time.Millisecond)
	s.ActiveClients.DeleteClient(client)

	if err := <-done; err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if len(stream.sent) != 1 || stream.sent[0].CxxExitCode != 1 {
		t.Fatalf("got %v, want one reply with exit code 1", stream.sent)
	}
}

// A client gives up on a broken stream and opens a new one, but the server may not notice for a while.
// Meanwhile the old stream must not take ready sessions: a .o sent there never reaches the client,
// which waited for it until forceInterruptTimeout (8 minutes) while the build stood still.
func TestAbandonedStreamLeavesSessionsToTheNextOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		// abandon makes the first stream's client give up on it, and makes a session ready
		abandon func(stream *fakeObjStream, cancel context.CancelFunc, readySession func(), start func())
	}{
		{"given up while waiting, then a session gets ready", func(stream *fakeObjStream, cancel context.CancelFunc, readySession func(), start func()) {
			start()
			time.Sleep(50 * time.Millisecond)
			cancel()
			time.Sleep(50 * time.Millisecond)
			readySession()
		}},
		{"given up with a session already waiting", func(stream *fakeObjStream, cancel context.CancelFunc, readySession func(), start func()) {
			cancel()
			readySession()
			start()
		}},
		{"the transport dies while the .o is being sent", func(stream *fakeObjStream, cancel context.CancelFunc, readySession func(), start func()) {
			stream.failSend = cancel
			readySession()
			start()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, client := newTestServerWithClient(t)
			defer s.ActiveClients.DeleteClient(client)
			objFile := filepath.Join(t.TempDir(), "a.o") // a sent session's .o is removed when it's closed
			if err := os.WriteFile(objFile, []byte("obj"), 0o644); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			abandoned := &fakeObjStream{ctx: ctx}
			abandonedDone := make(chan error, 1)
			tc.abandon(abandoned, cancel,
				func() { client.chanReadySessions <- &Session{sessionID: 9, client: client, objOutFile: objFile} },
				func() {
					go func() {
						abandonedDone <- s.RecvCompiledObjStream(&pb.OpenReceiveStreamRequest{ClientID: "c1"}, abandoned)
					}()
				})
			select {
			case <-abandonedDone:
			case <-time.After(time.Second):
				t.Fatal("the stream the client gave up on is still waiting for sessions")
			}

			next := &fakeObjStream{}
			go func() { _ = s.RecvCompiledObjStream(&pb.OpenReceiveStreamRequest{ClientID: "c1"}, next) }()
			deadline := time.Now().Add(time.Second)
			for len(next.sentSessionIDs()) == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if got := next.sentSessionIDs(); len(got) != 1 || got[0] != 9 {
				t.Fatalf("the client's next stream got sessions %v, want [9]: the abandoned stream lost it", got)
			}
		})
	}
}
