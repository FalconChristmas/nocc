package client

import (
	"fmt"
	"strconv"
	"testing"
)

// chooseRemoteConnectionForCppCompilation used to reduce the FNV hash in `int`:
//
//	daemon.remoteConnections[int(hasher.Sum32())%len(daemon.remoteConnections)]
//
// On a 32-bit client (armv7 -- BeagleBone, Raspberry Pi Zero/1) `int` is 32 bits,
// so that conversion is negative for every hash with the top bit set, which is
// roughly half of all file names. Go's % keeps the sign of the dividend, so with
// two servers configured the expression yielded -1 and panicked the daemon on the
// first such file; every later invocation then fell back to compiling locally.
// A single server masked it, because x%1 == 0 for any x.
//
// Run this with GOARCH=386 (or GOARCH=arm) to exercise 32-bit `int`; on a 64-bit
// host the old code cannot fail, so a 64-bit-only run proves nothing here.
func TestChooseRemoteConnectionForCppCompilationStaysInRange(t *testing.T) {
	if strconv.IntSize != 32 {
		t.Logf("int is %d bits here; the regression this guards only reproduces with a 32-bit int", strconv.IntSize)
	}

	for _, numRemotes := range []int{1, 2, 3, 4, 5, 8} {
		conns := make([]*RemoteConnection, numRemotes)
		for i := range conns {
			conns[i] = &RemoteConnection{remoteHost: fmt.Sprintf("host%d", i)}
		}
		daemon := &Daemon{remoteConnections: conns}

		seen := make(map[string]bool, numRemotes)
		for i := 0; i < 20000; i++ {
			cppInFile := fmt.Sprintf("src/file%d.cpp", i)
			remote := daemon.chooseRemoteConnectionForCppCompilation(cppInFile)
			if remote == nil {
				t.Fatalf("numRemotes=%d %s: got nil remote", numRemotes, cppInFile)
			}
			seen[remote.remoteHost] = true
		}
		if len(seen) != numRemotes {
			t.Errorf("numRemotes=%d: only %d of the remotes were ever chosen", numRemotes, len(seen))
		}
	}
}
