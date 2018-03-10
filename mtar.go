// Copyright 2018 Noel Cower
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are met:
//
// 1. Redistributions of source code must retain the above copyright notice,
//    this list of conditions and the following disclaimer.
//
// 2. Redistributions in binary form must reproduce the above copyright notice,
//    this list of conditions and the following disclaimer in the documentation
//    and/or other materials provided with the distribution.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
// AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
// IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
// ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
// LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
// CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
// SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
// INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
// CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
// ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
// POSSIBILITY OF SUCH DAMAGE.

// Mtar is a simple tar program to create tar files with arbitrary path mappings from the source
// filesystem to tar paths. It only supports regular files, directories, and symlinks.
//
// Download and install with
//
//     go get -u go.spiff.io/mtar
//
// Usage:
//
//    mtar [-h|--help] [FILE|OPTION]
//
//    Writes a tar file to standard output.
//
//    FILE may be a regular filepath for a file, symlink, or directory. If
//    FILE contains a ':', the text after the colon is the path to write to
//    the tar file. For example, the following paths behave differently:
//
//      SRC
//          Add file SRC to the tar file as-is.
//      SRC:
//          Add file SRC to the tar file as-is.
//      SRC:DEST
//          Add file SRC as DEST to the tar file.
//
//    There is currently no SRC to accept standard input as a file (other
//    than, for example, using /dev/stdin).
//
//    In addition, options may be passed in the middle of file arguments to
//    control archive creation:
//
//      -h | --help
//        When passed as the first argument, print this usage text.
//      -Cdir | -C dir
//        Change to directory (relative to PWD at all times; -C. will reset
//        the current directory) for subsequent file additions.
//      -OREGEX | -O REGEX
//        Add a filter to reject output paths, after mapping, that do not
//        match the REGEX.
//      -oREGEX | -o REGEX
//        Select only output paths, after mapping, that match the REGEX.
//      -IREGEX | -I REGEX
//        Add a filter to reject input paths (as passed) that match the REGEX.
//      -iREGEX | -i REGEX
//        Add a filter to select only input paths that match the REGEX.
//      -Ri, -Ro, -R
//        Reset input, output, or all filters, respectively.
//
package main // import "go.spiff.io/mtar"

import (
	"archive/tar"
	"io"
	"log"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

type Args struct{ args []string }

type Matcher struct {
	rx   *regexp.Regexp
	want bool
}

func (m Matcher) matches(s string) bool {
	return m.rx.MatchString(s) == m.want
}

var startupDir string
var skipSrcGlobs []Matcher
var skipDestGlobs []Matcher

func (p *Args) Shift() (s string, ok bool) {
	if ok = len(p.args) > 0; ok {
		s, p.args = p.args[0], p.args[1:]
	}
	return
}

func usage() {
	io.WriteString(os.Stderr,
		`Usage: mtar [-h|--help] [FILE|OPTION]

Writes a tar file to standard output.

FILE may be a regular filepath for a file, symlink, or directory. If
FILE contains a ':', the text after the colon is the path to write to
the tar file. For example, the following paths behave differently:

  SRC
      Add file SRC to the tar file as-is.
  SRC:
      Add file SRC to the tar file as-is.
  SRC:DEST
      Add file SRC as DEST to the tar file.

There is currently no SRC to accept standard input as a file (other
than, for example, using /dev/stdin).

In addition, options may be passed in the middle of file arguments to
control archive creation:

  -h | --help
    When passed as the first argument, print this usage text.
  -Cdir | -C dir
    Change to directory (relative to PWD at all times; -C. will reset
    the current directory) for subsequent file additions.
  -OREGEX | -O REGEX
    Add a filter to reject output paths, after mapping, that do not
    match the REGEX.
  -oREGEX | -o REGEX
    Select only output paths, after mapping, that match the REGEX.
  -IREGEX | -I REGEX
    Add a filter to reject input paths (as passed) that match the REGEX.
  -iREGEX | -i REGEX
    Add a filter to select only input paths that match the REGEX.
  -Ri, -Ro, -R
    Reset input, output, or all filters, respectively.`+"\n")
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("mtar: ")

	var err error
	startupDir, err = os.Getwd()
	failOnError("getwd", err)

	// Using some pretty weird CLI arguments here so incoming weird as hell arg loop ahead
	if len(os.Args) <= 1 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		usage()
		os.Exit(2)
	}

	w := tar.NewWriter(os.Stdout)
	defer func() { failOnError("error writing output", w.Close()) }()
	argv := Args{args: os.Args[1:]}
	for s, ok := argv.Shift(); ok; s, ok = argv.Shift() {
		switch {
		// Filter flags
		case s == "-Ro": // reset output filters
			skipSrcGlobs = nil
		case s == "-Ri": // reset input filters
			skipSrcGlobs = nil
		case s == "-R": // reset all filters
			skipSrcGlobs, skipDestGlobs = nil, nil
		case s == "-i" || s == "-I": // filter input by regexp
			want := s[1] == 'i'
			if s, ok = argv.Shift(); !ok {
				log.Fatal("-i: missing regexp")
			}
			skipSrcGlobs = append(skipSrcGlobs, Matcher{rx: regexp.MustCompile(s), want: want})
		case strings.HasPrefix(s, "-I") || strings.HasPrefix(s, "-i"):
			want := s[1] == 'i'
			skipSrcGlobs = append(skipSrcGlobs, Matcher{rx: regexp.MustCompile(s[2:]), want: want})
		case s == "-o" || s == "-O": // filter output by regexp (after mapping)
			want := s[1] == 'o'
			if s, ok = argv.Shift(); !ok {
				log.Fatal("-O: missing regexp")
			}
			skipDestGlobs = append(skipDestGlobs, Matcher{rx: regexp.MustCompile(s), want: want})
		case strings.HasPrefix(s, "-O") || strings.HasPrefix(s, "-o"):
			want := s[1] == 'o'
			skipDestGlobs = append(skipDestGlobs, Matcher{rx: regexp.MustCompile(s[2:]), want: want})

		// Change dir
		case s == "-C": // cd
			if s, ok = argv.Shift(); !ok {
				log.Fatal("-C: missing directory")
			}
			failOnError("cd", changeDir(s))
			continue

		case strings.HasPrefix(s, "-C"): // cd
			failOnError("cd", changeDir(s[2:]))

		// Add files
		default:
			src, dest := s, ""
			switch idx := strings.IndexByte(src, ':'); idx {
			case -1: // no mapping -- use src as path
			case 0: // no src
				log.Fatalf("no source: %q", s)
			case len(src) - 1: // no dest -- use src path
				src = s[:idx]
			default: // path given
				src, dest = s[:idx], s[idx+1:]
			}

			addFile(w, src, dest, true)
		}
	}
}

func addFile(w *tar.Writer, src, dest string, allowRecursive bool) {
	if shouldSkip(skipSrcGlobs, src) {
		return
	}

	st, err := os.Lstat(src)
	failOnError("add file: stat error", err)
	if dest == "" {
		dest = src
	}

	dest = path.Clean(filepath.ToSlash(dest))
	if strings.HasPrefix(dest, "/") {
		dest = "." + dest
	} else if !strings.HasPrefix("./", dest) {
		dest = "./" + dest
	}
	if dest == ".." || strings.HasPrefix(dest, "../") {
		log.Fatal("add file: destination may not contain .. (", dest, ")")
	}

	hdr := &tar.Header{
		Name:     dest,
		Typeflag: tar.TypeReg,
		ModTime:  st.ModTime(),
		Mode:     int64(st.Mode().Perm()),
		Format:   tar.FormatPAX,
	}

	if uid, gid, ok := getUidGid(st); ok {
		hdr.Uid, err = strconv.Atoi(uid.Uid)
		hdr.Uname = uid.Username
		if err != nil {
			log.Fatalf("cannot parse uid (%q) for %s: %v", uid.Uid, src, err)
		}
		hdr.Gid, err = strconv.Atoi(gid.Gid)
		hdr.Gname = gid.Name
		if err != nil {
			log.Fatalf("cannot parse gid (%q) for %s: %v", gid.Gid, src, err)
		}
	}

	switch {
	case st.Mode().IsRegular():
		hdr.Size = st.Size()
	case st.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name = dest + "/"
	case st.Mode()&os.ModeSymlink == os.ModeSymlink:
		hdr.Typeflag = tar.TypeSymlink
		hdr.Name = dest
		link, err := os.Readlink(src)
		failOnError("cannot resolve symlink", err)
		hdr.Linkname = link
	default:
		log.Print("skipping file: ", src, ": cannot add file with mode ", st.Mode().Perm())
		return
	}

	if shouldSkip(skipDestGlobs, hdr.Name) {
		return
	}

	failOnError("write header: "+hdr.Name, w.WriteHeader(hdr))

	if st.Mode().IsDir() {
		if allowRecursive {
			addRecursive(w, src, dest)
		}
		return
	}

	if !st.Mode().IsRegular() {
		return
	}

	file, err := os.Open(src)
	failOnError("read error: "+src, err)
	defer file.Close()
	n, err := io.Copy(w, file)
	failOnError("copy error: "+src, err)
	if n != hdr.Size {
		log.Fatalf("copy error: size mismatch for %s: wrote %d, want %d", src, n, hdr.Size)
	}
}

func addRecursive(w *tar.Writer, src, prefix string) {
	src = strings.TrimRight(src, "/")
	filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if filepath.Clean(p) == filepath.Clean(src) || shouldSkip(skipSrcGlobs, p) {
			return nil
		}
		dest := path.Join(prefix, strings.TrimPrefix(p, src))
		addFile(w, p, dest, false)
		return nil
	})
}

func changeDir(dir string) error {
	const pathsep = string(filepath.Separator)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(startupDir, dir)
	}
	return os.Chdir(dir)
}

func failOnError(prefix string, err error) {
	if err != nil {
		log.Fatalf("%s: %v", prefix, err)
	}
}

func shouldSkip(set []Matcher, s string) bool {
	for _, m := range set {
		if !m.matches(s) {
			return true
		}
	}
	return false
}

func getUidGid(f os.FileInfo) (*user.User, *user.Group, bool) {
	stat, ok := f.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, nil, false
	}
	uid, gid := strconv.FormatUint(uint64(stat.Uid), 10), strconv.FormatUint(uint64(stat.Gid), 10)
	u, err := user.LookupId(uid)
	if err != nil {
		return nil, nil, false
	}
	g, err := user.LookupGroupId(gid)
	if err != nil {
		return nil, nil, false
	}
	return u, g, true
}
