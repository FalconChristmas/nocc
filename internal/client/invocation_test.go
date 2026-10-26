package client

import (
	"path/filepath"
	"strings"
	"testing"
)

// newTestDaemon returns a Daemon that ParseCmdLineInvocation can be run against
// without a compiler on the box: pre-seeding includesCache keeps
// GetOrCreateIncludesCache from shelling out to detect default include dirs.
func newTestDaemon(cxxName string) *Daemon {
	return &Daemon{
		includesCache: map[string]*IncludesCache{
			cxxName: {
				cxxName:         cxxName,
				includesResolve: make(map[string]string),
				hFilesInfo:      make(map[string]*includeCachedHFile),
			},
		},
		cxxTargetTriplets: map[string]string{
			// pre-seeded, so parsing a command line in a test never execs the real compiler
			cxxName: "x86_64-test-linux-gnu",
		},
		cxxVersions: map[string]string{
			cxxName: "14.2.0",
		},
	}
}

// The daemon is a long-lived, machine-wide process: it keeps whatever working
// directory the `nocc` invocation that happened to start it was run from, and
// then serves invocations from every other directory on the box. So a relative
// -o must be resolved against the cwd sent with the request, never against the
// daemon's own. It used to be used verbatim, which wrote the .o into the tree of
// whichever build started the daemon -- silently, when that relative path
// happened to exist there.
func TestObjOutFileIsResolvedAgainstTheRequestCwd(t *testing.T) {
	daemon := newTestDaemon("g++")

	for _, c := range []struct {
		cwd    string
		oFlag  string
		expect string
	}{
		{"/build/projA", "src/1.o", "/build/projA/src/1.o"},
		{"/build/projB", "1.o", "/build/projB/1.o"},
		{"/build/projB", "./sub/1.o", "/build/projB/sub/1.o"},
		{"/build/projB", "/abs/elsewhere/1.o", "/abs/elsewhere/1.o"},
	} {
		invocation := ParseCmdLineInvocation(daemon, c.cwd, []string{"g++", "-c", "1.cpp", "-o", c.oFlag})
		if invocation.err != nil {
			t.Fatalf("cwd=%s -o %s: parse failed: %v", c.cwd, c.oFlag, invocation.err)
		}
		if got := invocation.GetObjOutFileAbs(); got != c.expect {
			t.Errorf("cwd=%s -o %s: got %s, want %s", c.cwd, c.oFlag, got, c.expect)
		}
		// the as-specified string has to survive: it is what the compiler writes as
		// the depfile's target, and what make matches its rule target against
		if invocation.objOutFile != c.oFlag {
			t.Errorf("cwd=%s -o %s: objOutFile was rewritten to %s", c.cwd, c.oFlag, invocation.objOutFile)
		}
	}
}

// Two builds served by one daemon must not collide, whatever it was started from.
func TestTwoCwdsDoNotShareAnOutputPath(t *testing.T) {
	daemon := newTestDaemon("g++")

	a := ParseCmdLineInvocation(daemon, "/build/projA", []string{"g++", "-c", "1.cpp", "-o", "sub/1.o"})
	b := ParseCmdLineInvocation(daemon, "/build/projB", []string{"g++", "-c", "1.cpp", "-o", "sub/1.o"})

	if a.GetObjOutFileAbs() == b.GetObjOutFileAbs() {
		t.Fatalf("both invocations resolved to %s", a.GetObjOutFileAbs())
	}
	if !strings.HasPrefix(a.GetObjOutFileAbs(), "/build/projA/") || !strings.HasPrefix(b.GetObjOutFileAbs(), "/build/projB/") {
		t.Errorf("resolved to %s and %s", a.GetObjOutFileAbs(), b.GetObjOutFileAbs())
	}
}

// -MMD is an alternative to -MD, not a modifier on it: both emit a depfile, and
// they differ only in whether system headers are listed. nocc strips the dep
// flags off cxxArgs and writes the file itself, so if this predicate is false
// nothing writes it -- "-MMD -MP" builds silently got no .d files at all.
func TestMMDAloneGeneratesADepFile(t *testing.T) {
	daemon := newTestDaemon("g++")

	for _, c := range []struct {
		args   []string
		expect bool
	}{
		{[]string{"-MMD", "-MP"}, true},
		{[]string{"-MMD"}, true},
		{[]string{"-MD"}, true},
		{[]string{"-MF", "out/1.o.d"}, true},
		{[]string{"-MP"}, false},
		{nil, false},
	} {
		cmdLine := append([]string{"g++", "-c", "1.cpp", "-o", "out/1.o"}, c.args...)
		invocation := ParseCmdLineInvocation(daemon, "/build/proj", cmdLine)
		if invocation.err != nil {
			t.Fatalf("%v: parse failed: %v", c.args, invocation.err)
		}
		if got := invocation.depsFlags.ShouldGenerateDepFile(); got != c.expect {
			t.Errorf("%v: ShouldGenerateDepFile() = %v, want %v", c.args, got, c.expect)
		}
	}
}

// Without -MF, the depfile sits next to the object -- which means it, too, has to
// be resolved against the request cwd and not the daemon's.
func TestDepFileNameIsAbsoluteAndNextToTheObject(t *testing.T) {
	daemon := newTestDaemon("g++")

	invocation := ParseCmdLineInvocation(daemon, "/build/proj", []string{"g++", "-c", "1.cpp", "-o", "src/1.o", "-MMD"})
	if got, want := invocation.depsFlags.calcOutputDepFileName(invocation), "/build/proj/src/1.d"; got != want {
		t.Errorf("without -MF: got %s, want %s", got, want)
	}

	invocation = ParseCmdLineInvocation(daemon, "/build/proj", []string{"g++", "-c", "1.cpp", "-o", "src/1.o", "-MMD", "-MF", "dep/1.o.d"})
	if got, want := invocation.depsFlags.calcOutputDepFileName(invocation), "/build/proj/dep/1.o.d"; got != want {
		t.Errorf("with -MF: got %s, want %s", got, want)
	}
	if !filepath.IsAbs(invocation.depsFlags.calcOutputDepFileName(invocation)) {
		t.Error("depfile name is not absolute")
	}
}

// A coverage object is only right if the server knows it is one (see server/coverage.go),
// so every spelling that turns on -fprofile-arcs or -ftest-coverage must be noticed, and an
// option that puts the coverage files somewhere else must keep the compilation local.
func TestCoverageOptionsAreNoticed(t *testing.T) {
	daemon := newTestDaemon("g++")

	for _, c := range []struct {
		args          []string
		arcs, notes   bool
		compileRemote bool
	}{
		{nil, false, false, true},
		{[]string{"--coverage"}, true, true, true},
		{[]string{"-coverage"}, true, true, true},
		{[]string{"-fprofile-arcs"}, true, false, true},
		{[]string{"-ftest-coverage"}, false, true, true},
		{[]string{"-fprofile-arcs", "-ftest-coverage"}, true, true, true},
		{[]string{"--coverage", "-fno-test-coverage"}, true, false, true},
		{[]string{"--coverage", "-fno-profile-arcs", "-fno-test-coverage"}, false, false, true},
		{[]string{"--coverage", "-fprofile-update=atomic", "-fprofile-abs-path"}, true, true, true},
		// where the coverage files go is the server's to decide; these would override it
		{[]string{"--coverage", "-fprofile-dir=/x"}, true, true, false},
		{[]string{"-fprofile-note=/x.gcno", "--coverage"}, true, true, false},
		{[]string{"--coverage", "-dumpbase", "x"}, true, true, false},
		{[]string{"--coverage", "-dumpdir", "x/"}, true, true, false},
		{[]string{"--coverage", "-save-temps"}, true, true, false},
		// harmless without coverage
		{[]string{"-dumpdir", "x/"}, false, false, true},
		// profile-guided optimization reads and writes profiles the server doesn't have
		{[]string{"-fprofile-generate"}, false, false, false},
		{[]string{"-fprofile-use=prof/"}, false, false, false},
	} {
		cmdLine := append([]string{"g++", "-c", "1.cpp", "-o", "out/1.o"}, c.args...)
		invocation := ParseCmdLineInvocation(daemon, "/build/proj", cmdLine)
		if (invocation.err == nil) != c.compileRemote {
			t.Errorf("%v: err = %v, want remote compilation %v", c.args, invocation.err, c.compileRemote)
			continue
		}
		if invocation.coverageArcs != c.arcs || invocation.coverageNotes != c.notes {
			t.Errorf("%v: arcs/notes = %v/%v, want %v/%v", c.args, invocation.coverageArcs, invocation.coverageNotes, c.arcs, c.notes)
		}
		if invocation.err == nil && invocation.IsCoverage() && invocation.cxxVersion != "14.2.0" {
			t.Errorf("%v: cxxVersion %q isn't sent", c.args, invocation.cxxVersion)
		}
	}

	// without its version, a server can't verify it's the same compiler, which a coverage object needs
	daemon.cxxVersions["g++"] = "" // like a compiler that doesn't answer -dumpfullversion
	invocation := ParseCmdLineInvocation(daemon, "/build/proj", []string{"g++", "--coverage", "-c", "1.cpp", "-o", "1.o"})
	if invocation.err == nil {
		t.Errorf("a coverage compilation with an unknown compiler version would be sent to a server")
	}
}

// The .gcno goes where gcc would put it: next to the object, named after it, ".o" replaced.
func TestNotesFileIsNamedAfterTheObject(t *testing.T) {
	daemon := newTestDaemon("g++")

	for _, c := range []struct {
		cwd, oFlag string
		args       []string
		want       string
	}{
		{"/build/proj", "src/foo.o", []string{"--coverage"}, "/build/proj/src/foo.gcno"},
		{"/build/proj", "CMakeFiles/x.dir/foo.cpp.o", []string{"--coverage"}, "/build/proj/CMakeFiles/x.dir/foo.cpp.gcno"},
		{"/build/proj", "/abs/out/foo.o", []string{"-ftest-coverage"}, "/abs/out/foo.gcno"},
		{"/build/proj", "src/foo.o", []string{"-fprofile-arcs"}, ""}, // no .gcno without -ftest-coverage
	} {
		invocation := ParseCmdLineInvocation(daemon, c.cwd, append([]string{"g++", "-c", "foo.cpp", "-o", c.oFlag}, c.args...))
		if invocation.err != nil {
			t.Fatalf("%v: parse failed: %v", c, invocation.err)
		}
		if got := invocation.GetNotesOutFileAbs(); got != c.want {
			t.Errorf("-o %s %v: notes file %q, want %q", c.oFlag, c.args, got, c.want)
		}
	}
}
