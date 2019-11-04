package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/crfs/stargz"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"golang.org/x/sys/unix"
)

func TestWhiteout(t *testing.T) {
	tests := []struct {
		name string
		in   []tarEntry
		want []fsCheck
	}{
		{
			name: "1_whiteout_with_sibling",
			in: tarOf(
				dir("foo/"),
				regfile("foo/bar.txt", ""),
				regfile("foo/.wh.foo.txt", ""),
			),
			want: checks(
				hasValidWhiteout("foo/foo.txt"),
				fileNotExist("foo/.wh.foo.txt"),
			),
		},
		{
			name: "1_whiteout_with_duplicated_name",
			in: tarOf(
				dir("foo/"),
				regfile("foo/bar.txt", "test"),
				regfile("foo/.wh.bar.txt", ""),
			),
			want: checks(
				hasFileDigest("foo/bar.txt", digestFor("test")),
				fileNotExist("foo/.wh.bar.txt"),
			),
		},
		{
			name: "1_opaque",
			in: tarOf(
				dir("foo/"),
				regfile("foo/.wh..wh..opq", ""),
			),
			want: checks(
				hasNodeXattrs("foo/", opaqueXattr, opaqueXattrValue),
				fileNotExist("foo/.wh..wh..opq"),
			),
		},
		{
			name: "1_opaque_with_sibling",
			in: tarOf(
				dir("foo/"),
				regfile("foo/.wh..wh..opq", ""),
				regfile("foo/bar.txt", "test"),
			),
			want: checks(
				hasNodeXattrs("foo/", opaqueXattr, opaqueXattrValue),
				hasFileDigest("foo/bar.txt", digestFor("test")),
				fileNotExist("foo/.wh..wh..opq"),
			),
		},
		{
			name: "1_opaque_with_xattr",
			in: tarOf(
				dir("foo/", xAttr{"foo": "bar"}),
				regfile("foo/.wh..wh..opq", ""),
			),
			want: checks(
				hasNodeXattrs("foo/", opaqueXattr, opaqueXattrValue),
				hasNodeXattrs("foo/", "foo", "bar"),
				fileNotExist("foo/.wh..wh..opq"),
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, cancel := buildTarGz(t, tt.in)
			defer cancel()
			var stargzBuf bytes.Buffer
			w := stargz.NewWriter(&stargzBuf)
			if err := w.AppendTar(tr); err != nil {
				t.Fatalf("Append: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Writer.Close: %v", err)
			}
			b := stargzBuf.Bytes()

			r, err := stargz.Open(io.NewSectionReader(bytes.NewReader(b), 0, int64(len(b))))
			if err != nil {
				t.Fatalf("stargz.Open: %v", err)
			}
			root, ok := r.Lookup("")
			if !ok {
				t.Fatalf("failed to find root in stargz")
			}
			gr := &stargzReader{
				digest: "test",
				r:      r,
				cache:  &testCache{membuf: map[string]string{}, t: t},
			}
			rootNode := &node{
				Node: nodefs.NewDefaultNode(),
				gr:   gr,
				e:    root,
			}
			_ = nodefs.NewFileSystemConnector(rootNode, &nodefs.Options{
				NegativeTimeout: 0,
				AttrTimeout:     time.Second,
				EntryTimeout:    time.Second,
				Owner:           nil, // preserve owners.
			})
			for _, want := range tt.want {
				want.check(t, rootNode)
			}
		})
	}
}

func buildTarGz(t *testing.T, ents []tarEntry) (r io.Reader, cancel func()) {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for _, ent := range ents {
			if err := ent.appendTar(tw); err != nil {
				t.Errorf("building input tar: %v", err)
				pw.Close()
				return
			}
		}
		if err := tw.Close(); err != nil {
			t.Errorf("closing write of input tar: %v", err)
		}
		pw.Close()
		return
	}()
	return pr, func() { go pr.Close(); go pw.Close() }
}

func tarOf(s ...tarEntry) []tarEntry { return s }

func checks(s ...fsCheck) []fsCheck { return s }

type tarEntry interface {
	appendTar(*tar.Writer) error
}

type tarEntryFunc func(*tar.Writer) error

func (f tarEntryFunc) appendTar(tw *tar.Writer) error { return f(tw) }

func regfile(name, contents string) tarEntry {
	return tarEntryFunc(func(tw *tar.Writer) error {
		if strings.HasSuffix(name, "/") {
			return fmt.Errorf("bogus trailing slash in file %q", name)
		}
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0644,
			Size:     int64(len(contents)),
		}); err != nil {
			return err
		}
		_, err := io.WriteString(tw, contents)
		return err
	})
}

func dir(d string, opts ...interface{}) tarEntry {
	return tarEntryFunc(func(tw *tar.Writer) error {
		var xattrs xAttr
		for _, opt := range opts {
			if v, ok := opt.(xAttr); ok {
				xattrs = v
			} else {
				return fmt.Errorf("unsupported opt")
			}
		}
		name := string(d)
		if !strings.HasSuffix(name, "/") {
			panic(fmt.Sprintf("missing trailing slash in dir %q ", name))
		}
		return tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     name,
			Mode:     0755,
			Xattrs:   xattrs,
		})
	})
}

type xAttr map[string]string

type fsCheck interface {
	check(t *testing.T, root *node)
}

type fsCheckFn func(*testing.T, *node)

func (f fsCheckFn) check(t *testing.T, root *node) { f(t, root) }

func fileNotExist(file string) fsCheck {
	return fsCheckFn(func(t *testing.T, root *node) {
		ent, inode, err := getDirentAndNode(root, file)
		if err == nil || ent != nil || inode != nil {
			t.Errorf("Node %q exists", file)
		}
	})
}

func hasFileDigest(file string, digest string) fsCheck {
	return fsCheckFn(func(t *testing.T, root *node) {
		_, inode, err := getDirentAndNode(root, file)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", file, err)
		}
		n, ok := inode.Node().(*node)
		if !ok {
			t.Fatalf("entry %q isn't a normal node", file)
		}
		if n.e.Digest != digest {
			t.Fatalf("Digest(%q) = %q, want %q", file, n.e.Digest, digest)
		}
	})
}

func hasValidWhiteout(name string) fsCheck {
	return fsCheckFn(func(t *testing.T, root *node) {
		ent, inode, err := getDirentAndNode(root, name)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", name, err)
		}
		n, ok := inode.Node().(*whiteout)
		if !ok {
			t.Fatalf("entry %q isn't a whiteout node", name)
		}
		var a fuse.Attr
		if status := n.GetAttr(&a, nil, nil); status != fuse.OK {
			t.Fatalf("failed to get attributes of file %q: %v", name, status)
		}
		if a.Ino != ent.Ino {
			t.Errorf("inconsistent inodes %d(Node) != %d(Dirent)", a.Ino, ent.Ino)
			return
		}

		// validate the direntry
		if ent.Mode != syscall.S_IFCHR {
			t.Errorf("whiteout entry %q isn't a char device", name)
			return
		}

		// validate the node
		if a.Mode != syscall.S_IFCHR {
			t.Errorf("whiteout %q has an invalid mode %o; want %o",
				name, a.Mode, syscall.S_IFCHR)
			return
		}
		if a.Rdev != uint32(unix.Mkdev(0, 0)) {
			t.Errorf("whiteout %q has invalid device numbers (%d, %d); want (0, 0)",
				name, unix.Major(uint64(a.Rdev)), unix.Minor(uint64(a.Rdev)))
			return
		}
	})
}

func hasNodeXattrs(entry, name, value string) fsCheck {
	return fsCheckFn(func(t *testing.T, root *node) {
		_, inode, err := getDirentAndNode(root, entry)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", entry, err)
		}
		n, ok := inode.Node().(*node)
		if !ok {
			t.Fatalf("entry %q isn't a normal node", entry)
		}

		// check xattr exists in the xattrs list.
		attrs, status := n.ListXAttr(nil)
		if status != fuse.OK {
			t.Fatalf("failed to get xattrs list of node %q: %v", entry, err)
		}
		var found bool
		for _, x := range attrs {
			if x == name {
				found = true
			}
		}
		if !found {
			t.Errorf("node %q doesn't have an opaque xattr %q", entry, value)
			return
		}

		// check the xattr has valid value.
		v, status := n.GetXAttr(name, nil)
		if status != fuse.OK {
			t.Fatalf("failed to get xattr %q of node %q: %v", name, entry, err)
		}
		if string(v) != value {
			t.Errorf("node %q has an invalid xattr %q; want %q", entry, v, value)
			return
		}
	})
}

// getDirentAndNode gets dirent and node at the specified path at once and makes
// sure that the both of them exist.
func getDirentAndNode(root *node, path string) (ent *fuse.DirEntry, n *nodefs.Inode, err error) {
	dir, base := filepath.Split(filepath.Clean(path))

	// get the target's parent directory.
	var attr fuse.Attr
	d := root
	for _, name := range strings.Split(dir, "/") {
		if len(name) == 0 {
			continue
		}
		di, status := d.Lookup(&attr, name, nil)
		if status != fuse.OK {
			err = fmt.Errorf("failed to lookup directory %q: %v", name, status)
			return
		}
		var ok bool
		if d, ok = di.Node().(*node); !ok {
			err = fmt.Errorf("directory %q isn't a normal node", name)
			return
		}

	}

	// get the target's direntry.
	var ents []fuse.DirEntry
	ents, status := d.OpenDir(nil)
	if status != fuse.OK {
		err = fmt.Errorf("failed to open directory %q: %v", path, status)
	}
	var found bool
	for _, e := range ents {
		if e.Name == base {
			ent, found = &e, true
		}
	}
	if !found {
		err = fmt.Errorf("direntry %q not found in the parent directory of %q", base, path)
	}

	// get the target's node.
	n, status = d.Lookup(&attr, base, nil)
	if status != fuse.OK {
		err = fmt.Errorf("failed to lookup node %q: %v", path, status)
	}

	return
}

func digestFor(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("sha256:%x", sum)
}
