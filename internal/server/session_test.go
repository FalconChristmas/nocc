package server

import (
	"strings"
	"testing"
)

func newTestSession(cppInFile string) *Session {
	return &Session{
		sessionID: 3,
		cppInFile: cppInFile,
		client:    &Client{clientID: "abc", workingDir: "/srv/nocc/cpp/clients/abc"},
	}
}

// Debug info names the compilation dir and the files as the compiler saw them: the server's mirror
// of the client's dirs, which gdb on the client can't find, unless they are mapped back.
func TestServerCmdLineMapsDebugInfoToTheClientsPaths(t *testing.T) {
	session := newTestSession("src/foo.cpp")
	noccServer := &NoccServer{ObjFileCache: &ObjFileCache{objTmpDir: "/srv/nocc/obj/cxx-out"}}
	session.PrepareServerCxxCmdLine(noccServer, "/home/fpp/proj", []string{"-g", "-ffile-prefix-map=/home/fpp/proj=."}, nil)

	cmdLine := strings.Join(session.cxxCmdLine, " ")
	debugMap := strings.Index(cmdLine, "-fdebug-prefix-map=/srv/nocc/cpp/clients/abc= ")
	clientMap := strings.Index(cmdLine, "-ffile-prefix-map=/srv/nocc/cpp/clients/abc/home/fpp/proj=.")
	if debugMap == -1 || clientMap == -1 {
		t.Fatalf("missing a prefix map: %s", cmdLine)
	}
	// gcc tries the maps last to first: the client's own must come after, to win
	if debugMap > clientMap {
		t.Errorf("the client's -ffile-prefix-map comes first, so the server's map overrides it: %s", cmdLine)
	}
	if strings.Contains(cmdLine, "-fprofile") || strings.Contains(cmdLine, "-dumpbase") {
		t.Errorf("coverage options in a compilation without coverage: %s", cmdLine)
	}
}

func TestServerCmdLineOfACoverageSession(t *testing.T) {
	session := newTestSession("src/foo.cpp")
	session.coverageObjFile = "/home/fpp/proj/build/foo.cpp.o"
	noccServer := &NoccServer{ObjFileCache: &ObjFileCache{objTmpDir: "/srv/nocc/obj/cxx-out"}}
	session.PrepareServerCxxCmdLine(noccServer, "/home/fpp/proj", []string{"--coverage"}, nil)

	cmdLine := strings.Join(session.cxxCmdLine, " ")
	for _, want := range []string{
		"-fprofile-prefix-map=/srv/nocc/cpp/clients/abc= ",
		"-dumpdir  -dumpbase foo.cpp -fprofile-dir=/home/fpp/proj/build -fprofile-prefix-path=/srv/nocc/cpp/clients/abc/home/fpp/proj",
		"-fprofile-note=/srv/nocc/obj/cxx-out/abc.3.gcno -o /srv/nocc/obj/cxx-out/abc.3.o src/foo.cpp",
	} {
		if !strings.Contains(cmdLine, want) {
			t.Errorf("no %q in %s", want, cmdLine)
		}
	}
	if session.cxxCwd != "/srv/nocc/cpp/clients/abc/home/fpp/proj" {
		t.Errorf("cxxCwd %s", session.cxxCwd)
	}
}
