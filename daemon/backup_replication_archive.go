package daemon

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

type replicationBoundWriter struct {
	w         io.Writer
	remaining int64
}

func (w *replicationBoundWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("backup archive exceeds max_bytes")
	}
	n, e := w.w.Write(p)
	w.remaining -= int64(n)
	return n, e
}
func replicationOpen(path string) (*os.File, os.FileInfo, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, nil, e
	}
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, errors.New("regular replication source required")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Getuid()) {
		f.Close()
		return nil, nil, errors.New("foreign replication file owner")
	}
	return f, info, nil
}
func (d *Daemon) prepareReplicationArchive(ctx context.Context, p BackupReplicationParams) (file *os.File, record BackupReplicationRecord, err error) {
	backup, err := d.readBackup(ctx, p.Backup)
	if err != nil {
		return nil, record, err
	}
	rootInfo, err := os.Stat(p.Backup)
	if err != nil {
		return nil, record, err
	}
	if st, ok := rootInfo.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Getuid()) || rootInfo.Mode().Perm() != 0700 {
		return nil, record, errors.New("private owned backup directory required")
	}
	if len(backup.Files) > 100000 {
		return nil, record, errors.New("backup has too many files")
	}
	marker, _, err := replicationOpen(filepath.Join(p.Backup, "verified.json"))
	if err != nil {
		return nil, record, err
	}
	manifestHash := sha256.New()
	_, err = io.Copy(manifestHash, io.LimitReader(marker, p.MaxBytes+1))
	closeErr := marker.Close()
	if err != nil || closeErr != nil {
		return nil, record, errors.Join(err, closeErr)
	}
	manifestSHA := hex.EncodeToString(manifestHash.Sum(nil))
	spool := filepath.Join(d.layout.Root, "backup-replication")
	if err = os.MkdirAll(spool, 0700); err != nil {
		return nil, record, err
	}
	physical, err := filepath.EvalSymlinks(spool)
	if err != nil || physical != spool {
		return nil, record, errors.New("replication spool must be physical")
	}
	spoolInfo, e := os.Lstat(spool)
	if e != nil {
		return nil, record, e
	}
	if st, ok := spoolInfo.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Getuid()) || !spoolInfo.IsDir() || spoolInfo.Mode().Perm() != 0700 {
		return nil, record, errors.New("private owned replication spool required")
	}
	remaining, err := d.backupQuotaRemaining(ctx)
	if err != nil {
		return nil, record, err
	}
	if remaining == 0 {
		return nil, record, errors.New("backup quota has no space for replication spool")
	}
	archiveLimit := p.MaxBytes
	if remaining > 0 && remaining < archiveLimit {
		archiveLimit = remaining
	}
	file, err = os.CreateTemp(spool, "archive-")
	if err != nil {
		return nil, record, err
	}
	created := file
	defer func() {
		if err != nil {
			created.Close()
			os.Remove(created.Name())
			file = nil
		}
	}()
	archiveHash := sha256.New()
	bounded := &replicationBoundWriter{io.MultiWriter(file, archiveHash), archiveLimit}
	tw := tar.NewWriter(bounded)
	names := make([]string, 0, len(backup.Files)+1)
	for name := range backup.Files {
		names = append(names, name)
	}
	names = append(names, "verified.json")
	sort.Strings(names)
	for _, name := range names {
		if err = ctx.Err(); err != nil {
			return nil, record, err
		}
		if name == "." || filepath.IsAbs(name) || filepath.Clean(name) != name || name == ".." || len(name) > 4096 || filepath.ToSlash(name) != name {
			return nil, record, errors.New("unsafe archive member")
		}
		source := filepath.Join(p.Backup, name)
		rel, e := filepath.Rel(p.Backup, source)
		if e != nil || rel != name || len(rel) >= 3 && rel[:3] == "../" {
			return nil, record, errors.New("archive member escapes backup")
		}
		parent, e := filepath.EvalSymlinks(filepath.Dir(source))
		if e != nil || parent != filepath.Dir(source) {
			return nil, record, errors.New("archive parent must be physical")
		}
		input, info, e := replicationOpen(source)
		if e != nil {
			return nil, record, e
		}
		if info.Size() > p.MaxBytes {
			input.Close()
			return nil, record, errors.New("archive member exceeds bound")
		}
		e = tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: info.Size(), Typeflag: tar.TypeReg, Format: tar.FormatPAX})
		h := sha256.New()
		if e == nil {
			_, e = io.Copy(io.MultiWriter(tw, h), &backupReader{ctx: ctx, r: input})
		}
		after, statErr := input.Stat()
		closeErr := input.Close()
		if e != nil || statErr != nil || closeErr != nil {
			return nil, record, errors.Join(e, statErr, closeErr)
		}
		if after.Size() != info.Size() || after.ModTime() != info.ModTime() {
			return nil, record, errors.New("backup member changed during archive")
		}
		expected := backup.Files[name]
		if name == "verified.json" {
			expected = manifestSHA
		}
		if hex.EncodeToString(h.Sum(nil)) != expected {
			return nil, record, fmt.Errorf("backup member checksum changed: %s", name)
		}
	}
	if err = tw.Close(); err != nil {
		return nil, record, err
	}
	if err = file.Sync(); err != nil {
		return nil, record, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, record, err
	}
	currentRoot, err := os.Stat(p.Backup)
	if err != nil || !os.SameFile(rootInfo, currentRoot) {
		return nil, record, errors.New("backup identity changed during archive")
	}
	if _, err = d.readBackup(ctx, p.Backup); err != nil {
		return nil, record, err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, record, err
	}
	record = BackupReplicationRecord{Backup: p.Backup, Endpoint: p.Endpoint, SHA256: hex.EncodeToString(archiveHash.Sum(nil)), ManifestSHA256: manifestSHA, Size: info.Size(), MaxBytes: p.MaxBytes, State: "pending"}
	return file, record, nil
}
