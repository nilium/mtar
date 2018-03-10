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
//    FILE may be a filepath for a file, symlink, or directory. If FILE
//    contains a ':', the text after the colon is the path to write to the tar
//    file. For example, the following paths behave differently:
//
//      SRC
//          Add file SRC to the tar file as-is.
//      SRC:
//          Add file SRC to the tar file as-is.
//      SRC:DEST
//          Add file SRC as DEST to the tar file.
//
//    To read a file from standard input, you can set '-' as the SRC. If no
//    DEST is given for this, it will default to dev/stdin (relative). File
//    permissions and ownership are taken from fd 1, so overriding them may be
//    necessary. If the link or dir option is set, - can be used to synthesize
//    a file entry.
//
//    In the case of SRC: and SRC:DEST, you can also pass an additional :OPTS
//    with either (i.e., SRC::OPTS or SRC:DEST:OPTS), where FLAGS are
//    comma-separated strings that set options. The following options are
//    available (all option names are case-sensitive):
//
//      norec
//        For directory entries, do not recursively add files from the
//        directory. This will cause only the directory itself to appear as
//        an entry.
//      dir
//        Force file to become a dir entry. Implies norec.
//      link=LINK
//        Force file to become a symlink pointing to LINK.
//      uid=UID | owner=USERNAME
//        Set the owner's uid and/or username for the file entry.
//      gid=GID | group=GROUPNAME
//        Set the gid and/or the group name for the file entry.
//      mode=MODE
//        Set the file mode to MODE (may be hex, octal, or an integer -- octal
//        must begin with a 0, hex with 0x).
//      mtime=TIME | atime=TIME | ctime=TIME
//        Sets the mod time, access time, or changed time to TIME. May be an
//        RFC3339 timestamp or an integer timestamp (since the Unix epoch) in
//        seconds, milliseconds (>=12 digits), or microseconds (>=15 digits).
//
//    Any whitespace preceding an option is trimmed. Whitespace is not trimmed
//    before or after the '=' symbol for options that take values. Commas are
//    not currently permitted inside options.
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
	"bytes"
	"errors"
	"fmt"
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
	"time"
)

type Args struct{ args []string }

type Matcher struct {
	rx   *regexp.Regexp
	want bool
}

func (m Matcher) matches(s string) bool {
	return m.rx.MatchString(s) == m.want
}

var startupTime = time.Now()
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

FILE may be a filepath for a file, symlink, or directory. If FILE
contains a ':', the text after the colon is the path to write to the tar
file. For example, the following paths behave differently:

  SRC
      Add file SRC to the tar file as-is.
  SRC:
      Add file SRC to the tar file as-is.
  SRC:DEST
      Add file SRC as DEST to the tar file.

To read a file from standard input, you can set '-' as the SRC. If no
DEST is given for this, it will default to dev/stdin (relative). File
permissions and ownership are taken from fd 1, so overriding them may be
necessary. If the link or dir option is set, - can be used to synthesize
a file entry.

In the case of SRC: and SRC:DEST, you can also pass an additional :OPTS
with either (i.e., SRC::OPTS or SRC:DEST:OPTS), where FLAGS are
comma-separated strings that set options. The following options are
available (all option names are case-sensitive):

  norec
    For directory entries, do not recursively add files from the
    directory. This will cause only the directory itself to appear as
    an entry.
  dir
    Force file to become a dir entry. Implies norec.
  link=LINK
    Force file to become a symlink pointing to LINK.
  uid=UID | owner=USERNAME
    Set the owner's uid and/or username for the file entry.
  gid=GID | group=GROUPNAME
    Set the gid and/or the group name for the file entry.
  mode=MODE
    Set the file mode to MODE (may be hex, octal, or an integer -- octal
    must begin with a 0, hex with 0x).
  mtime=TIME | atime=TIME | ctime=TIME
    Sets the mod time, access time, or changed time to TIME. May be an
    RFC3339 timestamp or an integer timestamp (since the Unix epoch) in
    seconds, milliseconds (>=12 digits), or microseconds (>=15 digits).

Any whitespace preceding an option is trimmed. Whitespace is not trimmed
before or after the '=' symbol for options that take values. Commas are
not currently permitted inside options.

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

	if argv.args[0] == "--" {
		argv.Shift()
	}

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

			var opts *FileOpts
			if idx := strings.IndexByte(dest, ':'); idx > -1 {
				opts, err = parseFileOptions(dest[idx+1:])
				failOnError("cannot parse options for "+src, err)
				dest = dest[:idx]
			}

			addFile(w, src, dest, opts, true)
		}
	}
}

func addFile(w *tar.Writer, src, dest string, opts *FileOpts, allowRecursive bool) {
	if shouldSkip(skipSrcGlobs, src) {
		return
	}

	var needBuffer bool
	var st os.FileInfo
	var err error

	if src == "-" {
		if dest == "" {
			dest = "dev/stdin"
		}
		st, err = os.Stdin.Stat()
		needBuffer = true
	} else {
		st, err = os.Lstat(src)
	}

	failOnError("add file: stat error", err)
	if dest == "" {
		dest = filepath.ToSlash(src)
		if strings.HasPrefix(dest, "/") {
			dest = path.Clean("." + dest)
		}
	}
	dest = path.Clean(filepath.ToSlash(dest))

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
	case st.Mode()&(os.ModeCharDevice|os.ModeDevice|os.ModeNamedPipe) != 0:
		needBuffer = true
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
		log.Print("skipping file: ", src, ": cannot add file")
		return
	}

	if shouldSkip(skipDestGlobs, hdr.Name) {
		return
	}

	opts.setHeaderFields(hdr)

	// Buffer input file if it's not a regular file
	var r io.Reader
	if needBuffer && hdr.Typeflag == tar.TypeReg {
		var file *os.File
		if src == "-" {
			file = os.Stdin
		} else {
			file, err = os.Open(src)
			failOnError("open error: "+src, err)
		}

		var buf bytes.Buffer
		_, err := io.Copy(&buf, file)
		failOnError("unable to buffer "+src, err)
		hdr.Size = int64(buf.Len())
		r = &buf

		if src != "-" {
			failOnError("unable to close "+src, file.Close())
		}
	}

	failOnError("write header: "+hdr.Name, w.WriteHeader(hdr))

	if st.Mode().IsDir() {
		if allowRecursive && opts.allowRecursive() {
			addRecursive(w, src, dest, opts)
		}
		return
	}

	if hdr.Typeflag != tar.TypeReg {
		return
	}

	if r == nil {
		file, err := os.Open(src)
		failOnError("read error: "+src, err)
		defer file.Close()
		r = file
	}
	n, err := io.Copy(w, r)
	failOnError("copy error: "+src, err)
	if n != hdr.Size {
		log.Fatalf("copy error: size mismatch for %s: wrote %d, want %d", src, n, hdr.Size)
	}

	failOnError("flush error: "+src, w.Flush())
}

func addRecursive(w *tar.Writer, src, prefix string, opts *FileOpts) {
	src = strings.TrimRight(src, "/")
	filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if filepath.Clean(p) == filepath.Clean(src) || shouldSkip(skipSrcGlobs, p) {
			return nil
		}
		dest := path.Join(prefix, strings.TrimPrefix(p, src))
		addFile(w, p, dest, opts, false)
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

type FileOpts struct {
	noRecursive bool

	// exclusive:
	dir  bool
	link string

	uid      *int
	username string

	gid   *int
	group string

	mode int64

	mtime time.Time
	atime time.Time
	ctime time.Time
}

func newFileOpts() *FileOpts {
	return &FileOpts{}
}

var timeLayouts = []string{
	time.RFC3339Nano,
	// TODO: add additional layouts if needed
}

func parseFileOptions(opts string) (*FileOpts, error) {
	fields := strings.FieldsFunc(opts, isComma)
	if len(fields) == 0 {
		return nil, nil
	}

	var err error
	fo := newFileOpts()
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		switch {
		case f == "norec":
			fo.noRecursive = true
		case f == "dir":
			if fo.link != "" {
				return nil, fmt.Errorf("may not set dir with link=%s", fo.link)
			}
			fo.dir = true
			fo.noRecursive = true
		case strings.HasPrefix(f, "link="):
			if fo.dir {
				return nil, errors.New("may not set link with dir")
			}
			if fo.link = f[len("link="):]; fo.link == "" {
				return nil, errors.New("may not set an empty link name")
			}
		case strings.HasPrefix(f, "uid="):
			if uid, err := strconv.Atoi(f[len("uid="):]); err != nil {
				return nil, fmt.Errorf("invalid uid: %v", err)
			} else {
				fo.uid = &uid
			}
		case strings.HasPrefix(f, "gid="):
			if gid, err := strconv.Atoi(f[len("gid="):]); err != nil {
				return nil, fmt.Errorf("invalid gid: %v", err)
			} else {
				fo.gid = &gid
			}
		case strings.HasPrefix(f, "owner="):
			if fo.username = f[len("owner="):]; fo.username == "" {
				return nil, errors.New("may not set an empty username for an owner")
			}
		case strings.HasPrefix(f, "group="):
			if fo.group = f[len("group="):]; fo.group == "" {
				return nil, errors.New("may not set an empty group name")
			}
		case strings.HasPrefix(f, "mode="):
			if fo.mode, err = strconv.ParseInt(f[len("mode="):], 0, 64); err != nil {
				return nil, fmt.Errorf("invalid mode: %v", err)
			} else if fo.mode == 0 {
				return nil, errors.New("invalid mode: may not be 0")
			}
		case strings.HasPrefix(f, "mtime=") || strings.HasPrefix(f, "atime=") || strings.HasPrefix(f, "ctime="):
			var tp *time.Time
			switch f[0] {
			case 'm':
				tp = &fo.mtime
			case 'a':
				tp = &fo.atime
			case 'c':
				tp = &fo.ctime
			}
			*tp = time.Time{}

			ts := f[len("mtime="):]
			if ts == "now" {
				*tp = startupTime
				continue
			}

			for _, layout := range timeLayouts {
				var t time.Time
				if t, err = time.Parse(layout, ts); err == nil {
					*tp = t
					break
				}
			}

			// Integer timestamp
			if err != nil {
				var ti int64
				ti, err = strconv.ParseInt(ts, 10, 64)
				if err != nil {
					goto timeFailure
				}
				// TODO: handle integer overflow
				if len(ts) >= 15 { // microseconds
					dur := time.Duration(ti) * time.Microsecond
					*tp = time.Unix(int64(dur/time.Second), int64(dur%time.Second))
				} else if len(ts) >= 12 { // milliseconds
					dur := time.Duration(ti) * time.Millisecond
					*tp = time.Unix(int64(dur/time.Second), int64(dur%time.Second))
				} else { // seconds
					*tp = time.Unix(ti, 0)
				}
			}
		timeFailure:
			if tp.IsZero() {
				return nil, fmt.Errorf("invalid %s: %q", f[:len("mtime")], ts)
			}
		default:
			return nil, fmt.Errorf("unexpected option: %q", f)
		}
	}

	return fo, nil
}

func (f *FileOpts) allowRecursive() bool {
	return f == nil || !f.noRecursive
}

func (f *FileOpts) setHeaderFields(hdr *tar.Header) {
	if f == nil {
		return
	}

	if f.uid != nil {
		hdr.Uid = *f.uid
	}
	if f.gid != nil {
		hdr.Gid = *f.gid
	}

	if f.username != "" {
		hdr.Uname = f.username
	}
	if f.group != "" {
		hdr.Gname = f.group
	}

	if f.mode != 0 {
		hdr.Mode = f.mode
	}

	if f.dir {
		hdr.Typeflag = tar.TypeDir
		hdr.Linkname = ""
		hdr.Size = 0
		if !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
	} else if f.link != "" {
		hdr.Linkname = f.link
		hdr.Typeflag = tar.TypeSymlink
	}

	if !f.mtime.IsZero() {
		hdr.ModTime = f.mtime
	}

	if !f.atime.IsZero() {
		hdr.AccessTime = f.atime
	}

	if !f.ctime.IsZero() {
		hdr.ChangeTime = f.ctime
	}
}

func isComma(r rune) bool {
	return r == ','
}
