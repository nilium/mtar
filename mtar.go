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
//      ref=LINK
//        Force file to become a hard link pointing to LINK.
//      nouser
//        Strip user information from the file.
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
//      -D
//        Prevent duplicate entries with the same name. (default)
//      -d
//        Allow duplicate entries with the same name.
//      -U
//        Do not assign user information to files.
//      -u
//        Assign user information to files. (default)
//      -Fformat | -F format
//        Set the tar header format to use. May be one of the following
//        formats:
//          * 'pax', '2001', 'posix.1-2001' (default)
//            Modern POSIX tar format.
//          * 'ustar', '1988', 'posix.1-1988'
//            The tar format written by most tar programs by default.
//            Does not support files over 8GiB.
//          * 'gnu'
//            A format specific to GNU tar archives.
//            Should not be chosen unless absolutely required.
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
//      -A
//        Read one or more tar streams from standard input and concatenate them
//        to the output.
//
package main // import "go.spiff.io/mtar"

import (
	"archive/tar"
	"bufio"
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

var (
	startupTime = time.Now()

	hdrFormat     = tar.FormatPAX
	skipSrcGlobs  []Matcher
	skipDestGlobs []Matcher
	skipUserInfo  bool
	skipWritten   = true
	written       = map[string]struct{}{} // Already-written paths
)

func (p *Args) Shift() (s string, ok bool) {
	if ok = len(p.args) > 0; ok {
		s, p.args = p.args[0], p.args[1:]
	}
	return
}

func usage() {
	_, _ = io.WriteString(os.Stderr,
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
  ref=LINK
    Force file to become a hard link pointing to LINK.
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
  -D
    Prevent duplicate entries with the same name. (default)
  -d
    Allow duplicate entries with the same name.
  -U
    Do not assign user information to files.
  -u
    Assign user information to files. (default)
  -Fformat | -F format
    Set the tar header format to use. May be one of the following
    formats:
      * 'pax', '2001', 'posix.1-2001' (default)
        Modern POSIX tar format.
      * 'ustar', '1988', 'posix.1-1988'
        The tar format written by most tar programs by default.
        Does not support files over 8GiB.
      * 'gnu'
        A format specific to GNU tar archives.
        Should not be chosen unless absolutely required.
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
    Reset input, output, or all filters, respectively.
  -A
    Read one or more tar streams from standard input and concatenate them
    to the output.`+"\n")
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("mtar: ")

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
		// Concatenate
		case s == "-A":
			catPath := ""
			if s, ok = argv.Shift(); ok {
				catPath = s
			}
			if err := concatenateTarFile(w, catPath); err != nil {
				log.Fatal("-A: error concatenating tar stream: ", err)
			}
		case strings.HasPrefix(s, "-A"):
			catPath := strings.TrimPrefix(s, "-A")
			if err := concatenateTarFile(w, catPath); err != nil {
				log.Fatal("-A: error concatenating tar stream: ", err)
			}

		// Set format
		case strings.HasPrefix(s, "-F"):
			fstr := strings.TrimPrefix(s, "-F")
			if fstr == "" {
				if fstr, ok = argv.Shift(); !ok {
					log.Fatal("-F: missing format (ustar, pax, gnu)")
				}
			}

			pred := hdrFormat
			switch strings.ToLower(fstr) {
			case "ustar", "1988", "posix.1-1988":
				hdrFormat = tar.FormatUSTAR
			case "pax", "2001", "posix.1-2001":
				hdrFormat = tar.FormatPAX
			case "gnu":
				hdrFormat = tar.FormatGNU
			default:
				log.Fatalf("-F: unrecognized format %q", fstr)
			}

			if pred != hdrFormat && len(written) > 0 {
				log.Printf("Warning: tar format changing mid-stream (%v -> %v)", pred, hdrFormat)
			}

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

		// -D  Skip duplicate header entries.
		// -d  Allow duplicate header entries.
		case s == "-D", s == "-d":
			skipWritten = s == "-D"

		// -U  Do not collect user info for headers unless explicitly set
		// -u  Enable collection.
		case s == "-U", s == "-u":
			skipUserInfo = s == "-U"

		// Change dir
		case s == "-C": // cd
			if s, ok = argv.Shift(); !ok {
				log.Fatal("-C: missing directory")
			}
			failOnError("cd", os.Chdir(s))
			continue

		case strings.HasPrefix(s, "-C"): // cd
			failOnError("cd", os.Chdir(s[2:]))

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

			opts := newFileOpts()
			if idx := strings.IndexByte(dest, ':'); idx > -1 {
				err := opts.parse(dest[idx+1:])
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

	var r io.Reader
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
		Format:   hdrFormat,
	}

	if uid, gid, ok := opts.getUidGid(st); ok {
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
		hdr.Name = dest
		link, err := os.Readlink(src)
		failOnError("cannot resolve symlink", err)
		if strings.HasPrefix(src, "/proc/self/fd/") && strings.HasPrefix(link, "pipe:[") && strings.HasSuffix(link, "]") { // Special case: <(proc) pipe
			needBuffer = true
			break
		}
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = link
	default:
		log.Print("skipping file: ", src, ": cannot add file")
		return
	}

	if shouldSkip(skipDestGlobs, hdr.Name) {
		return
	}

	opts.setHeaderFields(hdr)

	switch path.Clean(hdr.Name) {
	case "./", ".", "..", "/":
		if hdr.Typeflag == tar.TypeDir {
			goto addDirOnly
		}
		return
	}

	// Buffer input file if it's not a regular file
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
	written[hdr.Name] = struct{}{}

addDirOnly:
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

func concatenateTarFile(w *tar.Writer, src string) error {
	input := os.Stdin
	if src != "" && src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		input = f
	}
	r := bufio.NewReader(input)
	for {
		err := concatenateTarStream(w, r)
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
	}
}

func concatenateTarStream(w *tar.Writer, r *bufio.Reader) error {
	_, err := r.ReadByte()
	if err == io.EOF {
		return err
	}

	if err = r.UnreadByte(); err != nil {
		return fmt.Errorf("error unreading probe byte: %w", err)
	}

	t := tar.NewReader(r)
	for {
		hdr, err := t.Next()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("error reading tar header: %w", err)
		}

		dup := *hdr
		dup.Format = hdrFormat

		if skipUserInfo {
			dup.Gid, dup.Gname = 0, ""
			dup.Uid, dup.Uname = 0, ""
		}

		if shouldSkip(skipSrcGlobs, dup.Name) {
			continue
		}

		if err := w.WriteHeader(&dup); err != nil {
			return fmt.Errorf("error copying %q header from tar stream: %w", hdr.Name, err)
		}
		written[hdr.Name] = struct{}{}

		if hdr.Size > 0 {
			f := io.LimitReader(t, hdr.Size)
			if _, err := io.Copy(w, f); err != nil {
				return fmt.Errorf("error copying %q from tar stream: %w", hdr.Name, err)
			}
		}
	}
}

func addRecursive(w *tar.Writer, src, prefix string, opts *FileOpts) {
	src = strings.TrimRight(src, "/")
	src = filepath.Clean(src) + "/"
	_ = filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if info.IsDir() && !strings.HasSuffix(p, "/") {
			p += "/"
		}
		if p == src || shouldSkip(skipSrcGlobs, p) {
			return nil
		}
		dest := path.Join(prefix, strings.TrimPrefix(p, src))
		addFile(w, p, dest, opts, false)
		return nil
	})
}

func failOnError(prefix string, err error) {
	if err != nil {
		log.Fatalf("%s: %v", prefix, err)
	}
}

func shouldSkip(set []Matcher, s string) bool {
	if _, seen := written[s]; seen && skipWritten {
		return seen
	}
	for _, m := range set {
		if !m.matches(s) {
			return true
		}
	}
	return false
}

type FileOpts struct {
	noRecursive bool

	nouser bool
	user   *user.User
	group  *user.Group

	// exclusive:
	dir      bool
	link     string
	linkType byte

	mode int64

	mtime time.Time
	atime time.Time
	ctime time.Time
}

func newFileOpts() *FileOpts {
	return &FileOpts{
		nouser: skipUserInfo,
	}
}

var timeLayouts = []string{
	time.RFC3339Nano,
	// TODO: add additional layouts if needed
}

func (fo *FileOpts) parse(opts string) error {
	fields := strings.FieldsFunc(opts, isComma)
	if len(fields) == 0 {
		return nil
	}

	var err error
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
				return fmt.Errorf("may not set dir with link=%s", fo.link)
			}
			fo.dir = true
			fo.noRecursive = true
		case strings.HasPrefix(f, "link="):
			if fo.link != "" {
				return errors.New("link already assigned to file")
			}
			if fo.dir {
				return errors.New("may not set link with dir")
			}
			if fo.link = f[len("link="):]; fo.link == "" {
				return errors.New("may not set an empty link name")
			}
			fo.linkType = tar.TypeSymlink
		case strings.HasPrefix(f, "ref="):
			if fo.link != "" {
				return errors.New("link already assigned to file")
			}
			if fo.dir {
				return errors.New("may not set link with dir")
			}
			if fo.link = f[len("ref="):]; fo.link == "" {
				return errors.New("may not set an empty link name")
			}
			fo.linkType = tar.TypeLink
		case f == "nouser":
			fo.nouser = true
		case strings.HasPrefix(f, "uid="):
			fo.nouser = false
			uid := f[len("uid="):]
			fo.user, err = user.LookupId(uid)
			if err != nil {
				return fmt.Errorf("unable to lookup group by id %q: %v", uid, err)
			}
		case strings.HasPrefix(f, "gid="):
			fo.nouser = false
			gid := f[len("gid="):]
			fo.group, err = user.LookupGroupId(gid)
			if err != nil {
				return fmt.Errorf("unable to lookup group by id %q: %v", gid, err)
			}
		case strings.HasPrefix(f, "owner="):
			fo.nouser = false
			owner := f[len("owner="):]
			fo.user, err = user.Lookup(owner)
			if err != nil {
				return fmt.Errorf("unable to lookup owner by name %q: %v", owner, err)
			}
		case strings.HasPrefix(f, "group="):
			fo.nouser = false
			group := f[len("group="):]
			fo.group, err = user.LookupGroup(group)
			if err != nil {
				return fmt.Errorf("unable to lookup group by name %q: %v", group, err)
			}
		case strings.HasPrefix(f, "mode="):
			if fo.mode, err = strconv.ParseInt(f[len("mode="):], 0, 64); err != nil {
				return fmt.Errorf("invalid mode: %v", err)
			} else if fo.mode == 0 {
				return errors.New("invalid mode: may not be 0")
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
				return fmt.Errorf("invalid %s: %q", f[:len("mtime")], ts)
			}
		default:
			return fmt.Errorf("unexpected option: %q", f)
		}
	}

	if fo.user != nil && fo.group == nil {
		fo.group, err = user.LookupGroupId(fo.user.Gid)
		if err != nil {
			return fmt.Errorf("unable to look up group for uid %q: %v", fo.user.Uid, err)
		}
	}

	return nil
}

func (f *FileOpts) getUidGid(fi os.FileInfo) (userent *user.User, groupent *user.Group, ok bool) {
	ok = true
	if f != nil {
		if f.nouser {
			return nil, nil, false
		}
		userent = f.user
		groupent = f.group
	}

	if userent != nil && groupent != nil {
		return
	}

	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, nil, false
	}

	uid, gid := strconv.FormatUint(uint64(stat.Uid), 10), strconv.FormatUint(uint64(stat.Gid), 10)

	if userent == nil {
		u, err := user.LookupId(uid)
		if err != nil {
			return nil, nil, false
		}
		userent = u
	}

	if groupent == nil {
		g, err := user.LookupGroupId(gid)
		if err != nil {
			return nil, nil, false
		}
		groupent = g
	}

	return
}

func (f *FileOpts) allowRecursive() bool {
	return f == nil || !f.noRecursive
}

func (f *FileOpts) setHeaderFields(hdr *tar.Header) {
	if f == nil {
		return
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
		hdr.Typeflag = f.linkType
		hdr.Size = 0
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
