package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

// A --coverage compilation leaves two server paths behind that a local one never would:
//   - the .gcda path compiled into the object, which gcc forms from the absolute -o path
//     (a server's /tmp/nocc/obj/cxx-out/{clientID}.{sessionID}.o): the instrumented program would write
//     its counts there, on the client, under a name nothing can match to its object;
//   - the .gcno next to that -o, which a client never receives, and which records the compiler's cwd,
//     the server's mirror of the client's dir, that gcov resolves relative source names against.
// Neither -ffile-prefix-map nor -fprofile-prefix-map changes either of them (gcc 14).
//
// What does, with gcc 12+:
//   - "-dumpdir '' -dumpbase {base}" makes the aux name relative and named after the client's object;
//   - "-fprofile-dir={client obj dir} -fprofile-prefix-path={cxxCwd}" then turns the .gcda path into
//     {client obj dir}/{base}.gcda: gcc joins a relative aux name to its cwd, strips the prefix path
//     from it, and puts what remains (just {base}) under the profile dir;
//   - "-fprofile-note={file}" writes the .gcno where the server can find it;
//   - "-fprofile-prefix-map={workingDir}=" records header paths as the client's.
// The cwd inside the .gcno is the one thing no option reaches, so patchGcnoCwd rewrites it.
// The result is the same .o and .gcno a local compilation would produce.

// coverageNotesFileName is where the server's compiler writes the .gcno of a coverage session.
func coverageNotesFileName(objOutFile string) string {
	return strings.TrimSuffix(objOutFile, ".o") + ".gcno"
}

// coverageCxxArgs are added to a coverage compilation's cmd line, see the comment above.
// clientObjFile is the client's absolute path of the .o; cxxCwd is the compiler's cwd on the server.
func coverageCxxArgs(clientObjFile string, cxxCwd string, notesFile string) []string {
	return []string{
		"-dumpdir", "",
		"-dumpbase", strings.TrimSuffix(path.Base(clientObjFile), ".o"),
		"-fprofile-dir=" + path.Dir(clientObjFile),
		"-fprofile-prefix-path=" + cxxCwd,
		"-fprofile-note=" + notesFile,
	}
}

// coverageCxxEnv makes gcc's idea of its cwd exactly cxxCwd: gcc takes $PWD when it names the
// same directory as ".", and -fprofile-prefix-path only matches a cwd spelled like itself.
func coverageCxxEnv(cxxCwd string) []string {
	return append(os.Environ(), "PWD="+cxxCwd)
}

// patchGcnoCwd replaces the cwd recorded in a .gcno, which must be serverCwd, with clientCwd.
// The cwd is the first string after the header: magic, version, stamp, and since gcc 12 a checksum.
// gcc 12+ writes a string as a byte length (including the NUL) followed by the bytes; older ones write
// a length in 4-byte words followed by the NUL-padded bytes. Both layouts are tried at both header
// sizes, and exactly one must hold serverCwd: anything else is a format this doesn't know, and
// an error, rather than a .gcno quietly left pointing at the server.
func patchGcnoCwd(gcno []byte, serverCwd string, clientCwd string) ([]byte, error) {
	if len(gcno) < 4 {
		return nil, fmt.Errorf("not a .gcno file: %d bytes", len(gcno))
	}
	var order binary.AppendByteOrder
	switch string(gcno[:4]) {
	case "oncg":
		order = binary.LittleEndian
	case "gcno":
		order = binary.BigEndian
	default:
		return nil, fmt.Errorf("not a .gcno file: magic %q", gcno[:4])
	}

	type layout struct {
		offset      int  // of the length word
		lengthWords bool // gcc < 12
	}
	// the length word, the string, and its NUL (and padding)
	record := func(l layout, s string) []byte {
		length, size := len(s)+1, len(s)+1
		if l.lengthWords {
			length = (len(s) + 4) / 4
			size = length * 4
		}
		b := order.AppendUint32(nil, uint32(length))
		b = append(b, s...)
		return append(b, make([]byte, size-len(s))...)
	}

	var found []layout
	for _, offset := range []int{12, 16} {
		for _, lengthWords := range []bool{false, true} {
			l := layout{offset, lengthWords}
			if offset <= len(gcno) && bytes.HasPrefix(gcno[offset:], record(l, serverCwd)) {
				found = append(found, l)
			}
		}
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("can't find the cwd %q in the .gcno header (%d matches)", serverCwd, len(found))
	}

	l := found[0]
	patched := make([]byte, 0, len(gcno)-len(serverCwd)+len(clientCwd))
	patched = append(patched, gcno[:l.offset]...)
	patched = append(patched, record(l, clientCwd)...)
	patched = append(patched, gcno[l.offset+len(record(l, serverCwd)):]...)
	return patched, nil
}

// probeCoverage proves the whole mechanism on cxxName once, the way a session uses it: a compilation
// in a mirrored dir must produce an object naming the client's .gcda, and a .gcno whose cwd patches.
// A compiler that can't (clang, gcc before 12, a gcc that changed its format) is refused for coverage
// sessions, and the client compiles those elsewhere or locally; without the probe it would get back
// an object counting into the server's paths, or a compile error for an option it never passed.
func probeCoverage(cxxPath string) error {
	tmpDir, err := os.MkdirTemp("", "nocc-coverage-probe")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	workingDir := tmpDir + "/client"
	clientCwd := "/nocc-probe/src"
	clientObjFile := "/nocc-probe/obj/probe.cpp.o"
	cxxCwd := workingDir + clientCwd
	objOutFile := tmpDir + "/out.o"
	notesFile := coverageNotesFileName(objOutFile)
	if err := os.MkdirAll(cxxCwd, os.ModePerm); err != nil {
		return err
	}
	if err := os.WriteFile(cxxCwd+"/probe.cpp", []byte("int probe(int x) { return x ? 1 : 2; }\n"), 0o644); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmdLine := append([]string{"-fprofile-prefix-map=" + workingDir + "=", "--coverage", "-c"},
		coverageCxxArgs(clientObjFile, cxxCwd, notesFile)...)
	cmd := exec.CommandContext(ctx, cxxPath, append(cmdLine, "-o", objOutFile, "probe.cpp")...)
	cmd.Dir = cxxCwd
	cmd.Env = coverageCxxEnv(cxxCwd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("a coverage compilation failed: %v: %s", err, strings.TrimSpace(string(out)))
	}

	obj, err := os.ReadFile(objOutFile)
	if err != nil {
		return err
	}
	wantGcda := strings.TrimSuffix(clientObjFile, ".o") + ".gcda\x00"
	if !bytes.Contains(obj, []byte(wantGcda)) {
		return fmt.Errorf("the object doesn't name %s as its .gcda", strings.TrimSuffix(wantGcda, "\x00"))
	}
	gcno, err := os.ReadFile(notesFile)
	if err != nil {
		return fmt.Errorf("no .gcno written: %v", err)
	}
	_, err = patchGcnoCwd(gcno, cxxCwd, clientCwd)
	return err
}
