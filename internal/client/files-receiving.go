package client

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/VKCOM/nocc/internal/common"
	"github.com/VKCOM/nocc/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FilesReceiving is a singleton inside Daemon that holds a bunch of grpc streams to receive compiled .o files.
// The number of streams is limited, they all are initialized on daemon start.
// When another .o is ready, it's pushed by the server (a client only receives, it doesn't send anything back).
type FilesReceiving struct {
	daemon     *Daemon
	grpcClient *GRPCClient
}

func MakeFilesReceiving(daemon *Daemon, grpcClient *GRPCClient) *FilesReceiving {
	return &FilesReceiving{
		daemon:     daemon,
		grpcClient: grpcClient,
	}
}

func (fr *FilesReceiving) CreateReceiveStream() error {
	ctx, cancelFunc := context.WithCancel(context.Background())
	stream, err := fr.grpcClient.pb.RecvCompiledObjStream(ctx,
		&pb.OpenReceiveStreamRequest{ClientID: fr.daemon.clientID},
	)
	if err != nil {
		cancelFunc()
		return err
	}

	go fr.monitorRemoteStreamForObjReceiving(stream, cancelFunc)
	return nil
}

// RecreateReceiveStreamOrQuit replaces a broken stream.
// Unless the server ended the stream itself (objsMayBeLost is false then), .o files it had already pushed
// into it are lost: a server's Send only queues into the transport, so the server takes them as delivered
// and will never send them again. Nothing tells which invocations those were, so all of them on this remote
// fail now and get retried, rather than wait for forceInterruptTimeout.
func (fr *FilesReceiving) RecreateReceiveStreamOrQuit(failedStreamCancelFunc context.CancelFunc, err error, objsMayBeLost bool) {
	failedStreamCancelFunc() // will close the stream on the server also
	logClient.Error("recreate recv stream:", err)
	if objsMayBeLost {
		fr.daemon.OnRecvStreamBroke(fr.grpcClient, err)
	}
	time.Sleep(100 * time.Millisecond)

	if err := fr.CreateReceiveStream(); err != nil {
		fr.daemon.OnRemoteBecameUnavailable(fr.grpcClient, err)
	}
}

// monitorRemoteStreamForObjReceiving listens to a grpc receiving stream and handles .o files sent by a remote.
// When a next .o is ready on remote, it sends it to a stream.
// One stream is used to receive multiple .o files consecutively.
// If cxx compilation exits with non-zero code, the same stream is used to send error details.
// See RemoteConnection.WaitForCompiledObj.
func (fr *FilesReceiving) monitorRemoteStreamForObjReceiving(stream pb.CompilationService_RecvCompiledObjStreamClient, cancelFunc context.CancelFunc) {
	for {
		firstChunk, err := stream.Recv()

		// such complexity of error handling prevents hanging sessions and proper stream recreation
		if err != nil {
			// when a daemon quits, all streams are automatically closed
			select {
			case <-fr.daemon.quitChan:
				return
			default:
				break
			}
			// this connection was replaced by a reconnect, and its successor has its own stream
			if fr.grpcClient.IsCleared() {
				return
			}

			// grpc stream creation doesn't wait for ack, that's why
			// if a stream couldn't be created at all, we know this only on Recv() failure
			if st, ok := status.FromError(err); ok {
				if st.Code() == codes.Unauthenticated {
					fr.daemon.OnRemoteBecameUnavailable(fr.grpcClient, err)
					return
				}
			}

			// if something weird occurs, the server fails to send a chunk to a stream
			// it closes the stream and includes metadata to trailer
			// here, on the client size, we mark this invocation as errored, they'll be retried
			// a trailer arrives only with the server's own end of the stream, after everything it sent before,
			// so this session is the only one lost
			mdSession := stream.Trailer().Get("sessionID")
			if len(mdSession) == 1 {
				sessionID, _ := strconv.Atoi(mdSession[0])
				invocation := fr.daemon.FindBySessionID(uint32(sessionID))
				if invocation != nil {
					invocation.DoneRecvObj(err)
				}
				fr.RecreateReceiveStreamOrQuit(cancelFunc, err, false)
				return
			}

			// no trailer: the stream broke under the transport (a network blip, a keepalive timeout),
			// or with an rpc error that is not visible to the server-side (like codes.ResourceExhausted);
			// the server thinks that whatever .o it pushed was sent
			fr.RecreateReceiveStreamOrQuit(cancelFunc, err, true)
			return
		}

		invocation := fr.daemon.FindBySessionID(firstChunk.SessionID)
		if invocation == nil {
			logClient.Error("can't find invocation for obj", "sessionID", firstChunk.SessionID)
			if firstChunk.CxxExitCode == 0 {
				if err, _ = receiveObjFileByChunks(stream, firstChunk, "/tmp/nocc-dev-null", ""); err != nil {
					fr.RecreateReceiveStreamOrQuit(cancelFunc, err, true)
					return
				}
			}
			continue
		}

		// the same check as on the server, for a server that predates it: a compiler killed there
		// is a remote failure to retry, not a compile error
		if common.CompilerWasKilled(firstChunk.CxxExitCode, firstChunk.CxxStderr) {
			invocation.DoneRecvObj(fmt.Errorf("the C++ compiler was killed on the remote (exit code %d): %s", firstChunk.CxxExitCode, strings.TrimSpace(string(firstChunk.CxxStderr))))
			continue
		}

		invocation.cxxExitCode = int(firstChunk.CxxExitCode)
		invocation.cxxStdout = firstChunk.CxxStdout
		invocation.cxxStderr = firstChunk.CxxStderr
		invocation.cxxDuration = firstChunk.CxxDuration
		invocation.summary.nBytesReceived += int(firstChunk.FileSize + firstChunk.NotesFileSize)

		// non-zero cxxExitCode means a bug in cpp source code and doesn't require local fallback
		if firstChunk.CxxExitCode != 0 {
			invocation.DoneRecvObj(nil)
			continue
		}

		err, needRecreateStream := receiveObjFileByChunks(stream, firstChunk, invocation.GetObjOutFileAbs(), invocation.GetNotesOutFileAbs())
		// a coverage .o without its .gcno is useless, and would leave a stale .gcno from before next to it
		if err == nil && invocation.coverageNotes && firstChunk.NotesFileSize == 0 {
			_ = os.Remove(invocation.GetObjOutFileAbs())
			err = fmt.Errorf("the remote sent no .gcno for a coverage compilation")
		}
		invocation.DoneRecvObj(err)

		// recreate a stream if it's corrupted, like chunks mismatch
		// (if so, invocation won't be left hanged, as it's already errored)
		if err != nil && needRecreateStream {
			fr.RecreateReceiveStreamOrQuit(cancelFunc, err, true)
			return
		}

		// continue waiting for next .o files pushed by the remote over the same stream
	}
}

// receiveObjFileByChunks is an actual implementation of saving a server stream to a local client .o file.
// A coverage compilation's .gcno follows the .o in the same chunks (NotesFileSize of them); it's saved to
// notesOutFile, or dropped if that's "". See server.sendObjFileByChunks.
func receiveObjFileByChunks(stream pb.CompilationService_RecvCompiledObjStreamClient, firstChunk *pb.RecvCompiledObjChunkReply, objOutFile string, notesOutFile string) (error, bool) {
	receivedBytes := len(firstChunk.ChunkBody)
	objBytes := int(firstChunk.FileSize)
	expectedBytes := objBytes + int(firstChunk.NotesFileSize)

	var errWrite error
	var errRecv error

	if receivedBytes >= expectedBytes && firstChunk.NotesFileSize == 0 {
		// if a dir for objOutFile doesn't exist, it will fail; g++/clang act the same
		errWrite = os.WriteFile(objOutFile, firstChunk.ChunkBody, os.ModePerm)
		return errWrite, false
	}

	// the .o goes to a temp file as it arrives; the .gcno, being small, is collected and written after
	notes := make([]byte, 0, firstChunk.NotesFileSize)
	objLeft := objBytes
	fileTmp, errWrite := common.OpenTempFile(objOutFile)
	write := func(body []byte) {
		n := len(body)
		if n > objLeft {
			n = objLeft
		}
		if errWrite == nil {
			_, errWrite = fileTmp.Write(body[:n])
		}
		objLeft -= n
		notes = append(notes, body[n:]...)
	}
	write(firstChunk.ChunkBody)

	var nextChunk *pb.RecvCompiledObjChunkReply
	for receivedBytes < expectedBytes {
		nextChunk, errRecv = stream.Recv()
		if errRecv != nil { // EOF is also unexpected
			break
		}
		if nextChunk.SessionID != firstChunk.SessionID {
			errRecv = fmt.Errorf("inconsistent stream, chunks mismatch")
			break
		}
		write(nextChunk.ChunkBody)
		receivedBytes += len(nextChunk.ChunkBody)
	}

	// the .gcno before the .o: a build that sees the .o takes the compilation as done
	if errRecv == nil && errWrite == nil && notesOutFile != "" && len(notes) > 0 {
		errWrite = writeFileAtomically(notesOutFile, notes)
	}
	if fileTmp != nil {
		_ = fileTmp.Close()
		if errWrite == nil && errRecv == nil {
			errWrite = os.Rename(fileTmp.Name(), objOutFile)
		}
		_ = os.Remove(fileTmp.Name())
	}

	switch {
	case errRecv != nil:
		return errRecv, true // "true" to recreate recv stream
	case errWrite != nil:
		return errWrite, false // "false" means that the stream is ok, there was just a problem of saving a file
	default:
		return nil, false
	}
}

func writeFileAtomically(fileName string, body []byte) error {
	fileTmp, err := common.OpenTempFile(fileName)
	if err != nil {
		return err
	}
	_, err = fileTmp.Write(body)
	if errClose := fileTmp.Close(); err == nil {
		err = errClose
	}
	if err == nil {
		err = os.Rename(fileTmp.Name(), fileName)
	}
	if err != nil {
		_ = os.Remove(fileTmp.Name())
	}
	return err
}

// receiveLogFileByChunks gets a server log file and saves to a client file system, for debugging purposes
// (implementation is simpler than receiving obj file, don't bother with proper error handling).
// See server.sendLogFileByChunks.
func receiveLogFileByChunks(stream pb.CompilationService_DumpLogsClient, firstChunk *pb.DumpLogsReply, logOutFile string) (int, error) {
	receivedBytes := len(firstChunk.ChunkBody)

	outFile, err := os.OpenFile(logOutFile, os.O_CREATE|os.O_WRONLY, os.ModePerm)
	if err == nil {
		_, err = outFile.Write(firstChunk.ChunkBody)
	}

	var nextChunk *pb.DumpLogsReply
	for err == nil {
		nextChunk, err = stream.Recv()
		if err != nil || nextChunk.ChunkBody == nil { // nil chunk means end of file
			break
		}
		_, err = outFile.Write(nextChunk.ChunkBody)
		receivedBytes += len(nextChunk.ChunkBody)
	}

	if outFile != nil {
		_ = outFile.Close()
	}

	return receivedBytes, err // err remains nil on success
}
