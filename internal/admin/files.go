package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// Management operations require exclusive access to their input directories.
// OpenRoot confines reads even when an uncooperative process races a path check.
func cleanRelative(s string) bool {
	return s != "" && s != "." && path.Clean(s) == s && !path.IsAbs(s) && !strings.HasPrefix(s, "../") && !strings.ContainsAny(s, "\\\x00\r\n")
}

func noSymlinkPath(p string) error {
	absolute, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(absolute, current), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlinks_not_allowed")
		}
	}
	return nil
}

func openRegular(root *os.Root, name string) (*os.File, error) {
	if !cleanRelative(name) {
		return nil, errors.New("invalid_relative_path")
	}
	prefix := ""
	for _, part := range strings.Split(name, "/") {
		prefix = path.Join(prefix, part)
		st, err := root.Lstat(prefix)
		if err != nil {
			return nil, err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("symlinks_not_allowed")
		}
		if prefix == name && !st.Mode().IsRegular() {
			return nil, errors.New("regular_file_required")
		}
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("regular_file_required")
	}
	return f, nil
}

func digestReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), n, err
}
func digestBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func writeExclusive(p string, b []byte) error {
	if err := noSymlinkPath(filepath.Dir(p)); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(p)
		}
	}()
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = syncDirectory(filepath.Dir(p)); err != nil {
		return err
	}
	ok = true
	return nil
}

func replaceFile(p string, b []byte) error {
	if err := noSymlinkPath(p); err != nil {
		return err
	}
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() {
		return errors.New("regular_file_required")
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".apostille-atomic-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	// Preserve an operator-provisioned group-readable config across atomic
	// switches: the unprivileged container must still be able to read it.
	if owner, ok := st.Sys().(*syscall.Stat_t); ok {
		err = f.Chown(int(owner.Uid), int(owner.Gid))
	}
	if err == nil {
		err = f.Chmod(st.Mode().Perm())
	}
	if err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, p); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(p))
}

func syncDirectory(p string) error {
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

func readSmall(p string, limit int64) ([]byte, error) {
	if err := noSymlinkPath(p); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Dir(p))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := openRegular(root, filepath.Base(p))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("file_too_large")
	}
	return b, nil
}

func copyFile(root *os.Root, relative, dest string) (string, int64, error) {
	f, err := openRegular(root, relative)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return "", 0, err
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), f)
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	return hex.EncodeToString(h.Sum(nil)), n, err
}
