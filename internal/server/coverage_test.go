package server

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// gcnoHeader builds the start of a .gcno the way gcc writes it: magic, version, stamp,
// [checksum], then the cwd as a string record, followed by whatever comes next.
func gcnoHeader(order binary.AppendByteOrder, withChecksum bool, lengthWords bool, cwd string, rest []byte) []byte {
	b := []byte("oncg")
	if order == binary.BigEndian {
		b = []byte("gcno")
	}
	b = append(b, "B42*"...)
	b = order.AppendUint32(b, 0x12345678) // stamp
	if withChecksum {
		b = order.AppendUint32(b, 0)
	}
	data := append([]byte(cwd), 0)
	length := len(data)
	if lengthWords {
		length = (len(cwd) + 4) / 4
		data = append(data, make([]byte, length*4-len(data))...)
	}
	b = order.AppendUint32(b, uint32(length))
	b = append(b, data...)
	return append(b, rest...)
}

func TestPatchGcnoCwd(t *testing.T) {
	serverCwd := "/var/cache/nocc/cpp/clients/abc/home/fpp/src"
	clientCwd := "/home/fpp/src"
	rest := []byte{1, 0, 0, 0, 0xde, 0xad, 0xbe, 0xef} // support_unexecuted_blocks, then a record

	for _, c := range []struct {
		desc         string
		order        binary.AppendByteOrder
		withChecksum bool
		lengthWords  bool
	}{
		{"gcc 12+", binary.LittleEndian, true, false},
		{"gcc 8-11", binary.LittleEndian, false, true},
		{"gcc 12+ big-endian", binary.BigEndian, true, false},
		{"gcc 8-11 big-endian", binary.BigEndian, false, true},
	} {
		in := gcnoHeader(c.order, c.withChecksum, c.lengthWords, serverCwd, rest)
		want := gcnoHeader(c.order, c.withChecksum, c.lengthWords, clientCwd, rest)
		got, err := patchGcnoCwd(in, serverCwd, clientCwd)
		if err != nil {
			t.Errorf("%s: %v", c.desc, err)
		} else if !bytes.Equal(got, want) {
			t.Errorf("%s: patched\n%q\nwant\n%q", c.desc, got, want)
		}
	}
}

// A .gcno that doesn't hold the cwd the server compiled in is a format this doesn't know:
// it must be an error, not a .gcno sent back still pointing at the server.
func TestPatchGcnoCwdRefusesWhatItDoesntRecognize(t *testing.T) {
	serverCwd := "/var/cache/nocc/cpp/clients/abc/home/fpp/src"
	for desc, gcno := range map[string][]byte{
		"another cwd":  gcnoHeader(binary.LittleEndian, true, false, "/somewhere/else", nil),
		"a cwd prefix": gcnoHeader(binary.LittleEndian, true, false, serverCwd+"/sub", nil),
		"not a .gcno":  append([]byte("oncx"), gcnoHeader(binary.LittleEndian, true, false, serverCwd, nil)[4:]...),
		"truncated":    gcnoHeader(binary.LittleEndian, true, false, serverCwd, nil)[:24],
		"empty":        {},
		"header only":  gcnoHeader(binary.LittleEndian, true, false, serverCwd, nil)[:14],
	} {
		if _, err := patchGcnoCwd(gcno, serverCwd, "/home/fpp/src"); err == nil {
			t.Errorf("%s: patched without an error", desc)
		}
	}
}

func TestCoverageCxxArgsNameTheClientsFiles(t *testing.T) {
	args := strings.Join(coverageCxxArgs("/home/fpp/build/CMakeFiles/x.dir/foo.cpp.o", "/srv/c/home/fpp/build", "/srv/out/1.2.gcno"), " ")
	want := "-dumpdir  -dumpbase foo.cpp -fprofile-dir=/home/fpp/build/CMakeFiles/x.dir -fprofile-prefix-path=/srv/c/home/fpp/build -fprofile-note=/srv/out/1.2.gcno"
	if args != want {
		t.Errorf("got  %s\nwant %s", args, want)
	}
}

// The probe must be able to fail: a "compiler" that exits 0 and produces nothing usable is refused.
func TestCoverageProbeRefusesACompilerThatDoesntDoIt(t *testing.T) {
	binDir := t.TempDir()
	script := "#!/bin/sh\nfor a; do case $a in -o) o=1;; *) [ -n \"$o\" ] && echo junk > \"$a\"; o=;; esac; done\nexit 0\n"
	fake := filepath.Join(binDir, "test-nocoverage-g++")
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := probeCoverage(fake); err == nil {
		t.Errorf("a compiler that writes no instrumented object passed the probe")
	}
}

// The real thing, on whatever g++ this machine has: gcc 12+ must pass, and anything else
// (Apple's clang-as-g++, an older gcc) must be refused rather than produce wrong objects.
func TestCoverageProbeOnTheRealCompiler(t *testing.T) {
	cxxPath, err := exec.LookPath("g++")
	if err != nil {
		t.Skip("no g++ here")
	}
	versionOut, _ := exec.Command(cxxPath, "--version").Output()
	fullVersion, _ := exec.Command(cxxPath, "-dumpfullversion").Output()
	major, _ := strconv.Atoi(strings.SplitN(strings.TrimSpace(string(fullVersion)), ".", 2)[0])
	isGcc12 := !strings.Contains(string(versionOut), "clang") && major >= 12

	err = probeCoverage(cxxPath)
	if isGcc12 && err != nil {
		t.Errorf("gcc %d failed the probe: %v", major, err)
	}
	if !isGcc12 && err == nil {
		t.Errorf("%s passed the probe", strings.SplitN(string(versionOut), "\n", 2)[0])
	}

	if isGcc12 {
		cache := MakeCxxCapabilityCache()
		if err := cache.CheckCoverageMatchesClient("g++", strings.TrimSpace(string(fullVersion))); err != nil {
			t.Errorf("the same version was refused: %v", err)
		}
		if err := cache.CheckCoverageMatchesClient("g++", "1.2.3"); err == nil {
			t.Errorf("another version was accepted")
		}
	}
}
