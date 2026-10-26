package client

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// routing decisions are logged; without this, logClient is nil
	if err := MakeLoggerClient("", -1, true); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func newTestRemotes(n int) []*RemoteConnection {
	conns := make([]*RemoteConnection, n)
	for i := range conns {
		conns[i] = &RemoteConnection{remoteHost: fmt.Sprintf("host%d", i)}
	}
	return conns
}

// A server that can't be the compiler we asked for must not swallow its share of the build:
// those files go to another server, not to the (slow) local machine.
func TestFilesMoveOffAServerThatCantCompileThem(t *testing.T) {
	conns := newTestRemotes(3)
	daemon := &Daemon{remoteConnections: conns}
	conns[1].MarkIncapableOfCxx("arm-linux-gnueabihf-g++", "not installed there")

	sentToIncapable := 0
	sentToLocal := 0
	for i := 0; i < 3000; i++ {
		remote := daemon.chooseRemoteConnectionForCppCompilation(fmt.Sprintf("file%d.cpp", i), "arm-linux-gnueabihf-g++", false)
		if remote == nil {
			sentToLocal++
		} else if remote == conns[1] {
			sentToIncapable++
		}
	}
	if sentToIncapable != 0 || sentToLocal != 0 {
		t.Errorf("%d files went to the incapable server, %d fell back to local", sentToIncapable, sentToLocal)
	}
}

// Being unable to serve one compiler says nothing about another one.
func TestIncapabilityIsPerCompiler(t *testing.T) {
	conns := newTestRemotes(2)
	daemon := &Daemon{remoteConnections: conns}
	conns[0].MarkIncapableOfCxx("clang++", "not installed there")

	usedForGcc := make(map[*RemoteConnection]bool)
	for i := 0; i < 2000; i++ {
		usedForGcc[daemon.chooseRemoteConnectionForCppCompilation(fmt.Sprintf("file%d.cpp", i), "g++", false)] = true
	}
	if !usedForGcc[conns[0]] {
		t.Error("a server refused for clang++ is no longer used for g++ either")
	}
}

// Only the displaced files move. Everything else keeps its server, so the other servers'
// caches stay warm — the whole reason the choice is a hash in the first place.
func TestOtherFilesKeepTheirServer(t *testing.T) {
	conns := newTestRemotes(4)
	daemon := &Daemon{remoteConnections: conns}

	before := make([]*RemoteConnection, 2000)
	for i := range before {
		before[i] = daemon.chooseRemoteConnectionForCppCompilation(fmt.Sprintf("file%d.cpp", i), "g++", false)
	}

	conns[2].MarkIncapableOfCxx("g++", "not installed there")

	moved, movedOffTheIncapable := 0, 0
	for i := range before {
		after := daemon.chooseRemoteConnectionForCppCompilation(fmt.Sprintf("file%d.cpp", i), "g++", false)
		if after != before[i] {
			moved++
			if before[i] == conns[2] {
				movedOffTheIncapable++
			}
		}
	}
	if moved == 0 {
		t.Fatal("nothing moved off the incapable server")
	}
	if moved != movedOffTheIncapable {
		t.Errorf("%d files moved, but only %d of them were on the incapable server", moved, movedOffTheIncapable)
	}
}

func TestUnavailableServersAreSkippedToo(t *testing.T) {
	conns := newTestRemotes(2)
	daemon := &Daemon{remoteConnections: conns}
	conns[0].isUnavailable.Store(true)

	for i := 0; i < 500; i++ {
		if remote := daemon.chooseRemoteConnectionForCppCompilation(fmt.Sprintf("file%d.cpp", i), "g++", false); remote != conns[1] {
			t.Fatalf("file%d.cpp went to %v, not to the only available server", i, remote)
		}
	}
}

// With nothing left to compile on, the caller has to know — that's what makes it
// fall back to a local compilation instead of dialing a server that refused it.
func TestNoUsableServerReturnsNil(t *testing.T) {
	conns := newTestRemotes(2)
	daemon := &Daemon{remoteConnections: conns}
	conns[0].isUnavailable.Store(true)
	conns[1].MarkIncapableOfCxx("g++", "not installed there")

	if remote := daemon.chooseRemoteConnectionForCppCompilation("1.cpp", "g++", false); remote != nil {
		t.Errorf("got %v, want nil", remote)
	}
}

// A server that predates coverage support would return an object counting into its own paths:
// coverage compilations never go there, and everything else still does.
func TestCoverageOnlyGoesToServersThatSupportIt(t *testing.T) {
	conns := newTestRemotes(3)
	daemon := &Daemon{remoteConnections: conns}
	conns[0].supportsCoverage = true
	conns[2].supportsCoverage = true

	usedForCoverage := map[*RemoteConnection]bool{}
	usedForPlain := map[*RemoteConnection]bool{}
	for i := 0; i < 500; i++ {
		usedForCoverage[daemon.chooseRemoteConnectionForCppCompilation(fmt.Sprintf("file%d.cpp", i), "g++", true)] = true
		usedForPlain[daemon.chooseRemoteConnectionForCppCompilation(fmt.Sprintf("file%d.cpp", i), "g++", false)] = true
	}
	if usedForCoverage[conns[1]] || usedForCoverage[nil] || len(usedForCoverage) != 2 {
		t.Errorf("coverage compilations went to %v, want only the two servers that support it", usedForCoverage)
	}
	if len(usedForPlain) != 3 {
		t.Errorf("other compilations used %d servers, want all 3", len(usedForPlain))
	}

	// a server whose compiler is refused for coverage only (another version) keeps the rest of the build
	conns[0].MarkIncapableOfCxx(coverageCapabilityName("g++"), "version 13.3.0 here")
	for i := 0; i < 500; i++ {
		file := fmt.Sprintf("file%d.cpp", i)
		if remote := daemon.chooseRemoteConnectionForCppCompilation(file, "g++", true); remote != conns[2] {
			t.Fatalf("%s: coverage went to %v, not to the one server left for it", file, remote)
		}
		usedForPlain[daemon.chooseRemoteConnectionForCppCompilation(file, "g++", false)] = true
	}
	if !usedForPlain[conns[0]] {
		t.Errorf("a server refused for coverage got none of the other compilations")
	}
}
