package server

import (
	"sync/atomic"

	"github.com/VKCOM/nocc/internal/common"
)

// Session is created when a client requests to compile a .cpp file.
// It's a server representation of client.Invocation.
// A lifetime of one Session is the following:
// 1) a session is created, provided a .cpp file and all .h/.nocc-pch/etc. dependencies
// 2) files that don't exist on this server are uploaded by the client
// 3) the C++ compiler (cxx) is launched
// 4) the client downloads .o
// 5) the session is closed automatically
// Steps 2-5 can be skipped if a compiled .o already exists in ObjFileCache.
type Session struct {
	sessionID uint32

	cppInFile  string // as-is from a client cmd line (relative to cxxCwd on a server-side)
	objOutFile string // inside /tmp/nocc/obj/cxx-out, or directly in /tmp/nocc/obj/obj-cache if taken from cache
	cxxCwd     string // cwd for the C++ compiler on a server-side (= client.workingDir + clientCwd)
	cxxName    string // g++ / clang / etc.
	cxxCmdLine []string

	// set for a coverage compilation (see coverage.go): the client's absolute path of the .o
	coverageObjFile string
	// the .gcno it produced, its cwd patched to the client's; nil if the compiler wrote none (-fprofile-arcs alone)
	coverageNotes []byte
	// the .gcno couldn't be made right; the session fails, and the client compiles it again elsewhere
	coverageErr error

	client *Client
	files  []*fileInClientDir

	objCacheKey        common.SHA256
	objCacheExists     bool
	compilationStarted int32

	cxxExitCode int32
	cxxStdout   []byte
	cxxStderr   []byte
	cxxDuration int32
}

// PrepareServerCxxCmdLine prepares a command line for cxx invocation.
// Notably, options like -Wall and -fpch-preprocess are pushed as is,
// but include dirs like /home/alice/headers need to be remapped to point to server dir.
func (session *Session) PrepareServerCxxCmdLine(noccServer *NoccServer, clientCwd string, cxxArgs []string, cxxIDirs []string) {
	session.objOutFile = noccServer.ObjFileCache.GenerateObjOutFileName(session)

	var cppInFile string
	// old clients that don't send this field (they send abs cppInFile)
	// todo delete later, after upgrading all clients
	if clientCwd == "" {
		cppInFile = session.client.MapClientFileNameToServerAbs(session.cppInFile)
		session.cxxCwd = session.client.workingDir
	} else {
		// session.cppInFile is as-is from a client cmd line:
		// * "/abs/path" becomes "client.workingDir/abs/path"
		//    (except for system files, /usr/include left unchanged)
		// * "rel/path" (relative to clientCwd) is left as-is (becomes relative to session.cxxCwd)
		//    (for correct __FILE__ expansion and other minor specifics)
		if session.cppInFile[0] == '/' {
			cppInFile = session.client.MapClientFileNameToServerAbs(session.cppInFile)
		} else {
			cppInFile = session.cppInFile
		}
		session.cxxCwd = session.client.MapClientFileNameToServerAbs(clientCwd)
	}

	cxxCmdLine := make([]string, 0, len(cxxIDirs)+len(cxxArgs)+13)

	// the client's paths in debug info (the compilation dir and the files), not the server's mirror of them;
	// first, so that a client's own -ffile-prefix-map (rewritten below) takes precedence: gcc tries the last one first
	cxxCmdLine = append(cxxCmdLine, "-fdebug-prefix-map="+session.client.workingDir+"=")
	if session.coverageObjFile != "" {
		cxxCmdLine = append(cxxCmdLine, "-fprofile-prefix-map="+session.client.workingDir+"=")
	}

	// loop through -I {dir} / -include {file} / etc. (format is guaranteed), converting client {dir} to server path
	for i := 0; i < len(cxxIDirs); i += 2 {
		arg := cxxIDirs[i]
		serverIdir := session.client.MapClientFileNameToServerAbs(cxxIDirs[i+1])
		cxxCmdLine = append(cxxCmdLine, arg, serverIdir)
	}

	for i := 0; i < len(cxxArgs); i++ {
		cxxArg := FilePrefixMapOption(cxxArgs[i], session.client.workingDir)

		cxxCmdLine = append(cxxCmdLine, cxxArg)
	}
	if session.coverageObjFile != "" {
		cxxCmdLine = append(cxxCmdLine, coverageCxxArgs(session.coverageObjFile, session.cxxCwd, coverageNotesFileName(session.objOutFile))...)
	}
	// build final string
	session.cxxCmdLine = append(cxxCmdLine, "-o", session.objOutFile, cppInFile)
}

// StartCompilingObjIfPossible executes cxx if all dependent files (.cpp/.h/.nocc-pch/etc.) are ready.
// They have either been uploaded by the client or already taken from src cache.
// Note, that it's called for sessions that don't exist in obj cache.
func (session *Session) StartCompilingObjIfPossible(noccServer *NoccServer) {
	for _, file := range session.files {
		if file.state != fsFileStateUploaded {
			return
		}
	}

	if atomic.SwapInt32(&session.compilationStarted, 1) == 0 {
		go noccServer.CxxLauncher.LaunchCxxWhenPossible(noccServer, session)
	}
}

func (session *Session) PushToClientReadyChannel() {
	// a client could have disconnected while cxx was working, then chanDisconnected is closed
	select {
	case <-session.client.chanDisconnected:
	case session.client.chanReadySessions <- session:
		// note, that if this chan is full, this 'case' (and this function call) is blocking
	}
}
