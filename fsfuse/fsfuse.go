// Copyright 2023 Tamás Gulácsi. All rights reserved.
//
// SPDX-License-Identifier: Apache-2.0

// Package fsfuse exposes an fs.FS as a FUSE server.
package fsfuse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
)

type FS struct {
	*fuseutil.NotImplementedFileSystem
	fsys     fs.FS
	cacheDur time.Duration
	uid, gid uint32

	inodeSeq   uint64
	handleSeq  uint64
	generation uint64 // GenerationNumber - must be incremeneted on each inode reuse (or inde removal)

	mu             sync.RWMutex
	inodePaths     map[fuseops.InodeID]string
	pathInodes     map[string]fuseops.InodeID
	inodeRefCounts map[fuseops.InodeID]uint32
	files          map[fuseops.HandleID]fs.File
}

const DefaultCacheDur = 356 * 24 * time.Hour

// options zero value means the default: process' uid,gid, DefaultCacheDur
type options struct {
	uid, gid int32
	cacheDur time.Duration
}

// Option sets an option on options.
type Option func(*options)

// WithUid sets the uid for the mount.
func WithUid(uid uint32) Option { return func(o *options) { o.uid = int32(uid) - 1 } }

// WithGid sets the gid for the mount.
func WithGid(uid uint32) Option { return func(o *options) { o.uid = int32(uid) - 1 } }

// WithCacheDur sets the cache duration for the mount.
func WithCacheDur(dur time.Duration) Option { return func(o *options) { o.cacheDur = dur - 1 } }

// NewServer returns a fuse.Server for the given fs.FS.
func NewServer(fsys fs.FS, opts ...Option) fuse.Server {
	return fuseutil.NewFileSystemServer(New(fsys, opts...))
}
func fix[T int32 | time.Duration](i, d T) T {
	if i < 0 {
		return 0
	}
	if i == 0 {
		return d
	}
	return i + 1
}

// New returns an *FS, with the given Options.
//
// If cacheDur < 0 then the caching will be disabled;
// if cacheDur == 0 then the default 1 year will be used.
func New(fsys fs.FS, opts ...Option) *FS {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return &FS{
		fsys:           fsys,
		uid:            uint32(fix(o.uid, int32(os.Getuid()))),
		gid:            uint32(fix(o.gid, int32(os.Getuid()))),
		cacheDur:       fix(o.cacheDur, DefaultCacheDur),
		inodeSeq:       1,
		inodePaths:     map[fuseops.InodeID]string{1: "."},
		pathInodes:     map[string]fuseops.InodeID{"": 1, ".": 1},
		inodeRefCounts: map[fuseops.InodeID]uint32{1: 1},
		files:          make(map[fuseops.HandleID]fs.File),
	}
}

// Mount the fuse.Server at mountPath.
func Mount(ctx context.Context, f fuse.Server, mountPath string) (MountedFileSystem, error) {
	m, err := fuse.Mount(mountPath, f,
		&fuse.MountConfig{
			OpContext: ctx,
			ReadOnly:  true,
			//DebugLogger: log.Default(),
			ErrorLogger: log.Default(),
		},
	)
	if err != nil {
		return MountedFileSystem{}, err
	}
	return MountedFileSystem{MountedFileSystem: m, Path: mountPath}, nil
}

// Unmount the mountPath.
func Unmount(mountPath string) error { return fuse.Unmount(mountPath) }

// MountedFileSystem wraps a *fuse.MountedFileSystem to store the mount path.
type MountedFileSystem struct {
	*fuse.MountedFileSystem
	Path string
}

// Mount the FS on mountPath, with the given Context.
func (f *FS) Mount(ctx context.Context, mountPath string) (MountedFileSystem, error) {
	return Mount(ctx, fuseutil.NewFileSystemServer(f), mountPath)
}

// Unmount the MountedFileSystem, waiting for finish till Context.Deadline.
func (m MountedFileSystem) Unmount(ctx context.Context) error {
	if m.MountedFileSystem == nil {
		return nil
	}
	if err := fuse.Unmount(m.Path); err != nil {
		return err
	}
	return m.MountedFileSystem.Join(ctx)
}

func (f *FS) getPathInode(fn string) fuseops.InodeID {
	f.mu.RLock()
	inode, ok := f.pathInodes[fn]
	f.mu.RUnlock()
	if ok {
		return inode
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if inode, ok = f.pathInodes[fn]; ok {
		return inode
	}
	i := fuseops.InodeID(atomic.AddUint64(&f.inodeSeq, 1))
	f.pathInodes[fn] = i
	f.inodePaths[i] = fn
	return i
}

func (f *FS) infoAttributes(fi fs.FileInfo) fuseops.InodeAttributes {
	return fuseops.InodeAttributes{
		Size:  uint64(fi.Size()),
		Mode:  fi.Mode(),
		Atime: fi.ModTime(),
		Mtime: fi.ModTime(),
		Ctime: fi.ModTime(),
		Nlink: 1,
		Uid:   f.uid, Gid: f.gid,
	}
}

func (f *FS) LookUpInode(ctx context.Context, op *fuseops.LookUpInodeOp) error {
	f.mu.RLock()
	fn := path.Join(f.inodePaths[op.Parent], op.Name)
	f.mu.RUnlock()
	file, err := f.fsys.Open(fn)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %w", fuse.ENOENT, err)
		}
		return err
	}
	fi, err := file.Stat()
	file.Close()
	if err != nil {
		return fmt.Errorf("%w: %w", fuse.EIO, err)
	}
	op.Entry = fuseops.ChildInodeEntry{
		Child:      f.getPathInode(fn),
		Generation: fuseops.GenerationNumber(f.generation),
		Attributes: f.infoAttributes(fi),
	}
	f.inodeRefCounts[op.Entry.Child]++
	if f.cacheDur != 0 {
		op.Entry.AttributesExpiration = time.Now().Add(f.cacheDur)
		op.Entry.EntryExpiration = op.Entry.AttributesExpiration
	}
	return nil
}

func (f *FS) GetInodeAttributes(ctx context.Context, op *fuseops.GetInodeAttributesOp) error {
	f.mu.RLock()
	path := f.inodePaths[op.Inode]
	f.mu.RUnlock()
	if path == "" && op.Inode == 1 {
		path = "."
	}
	file, err := f.fsys.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %w", fuse.ENOENT, err)
		}
		return fmt.Errorf("%w: open %q: %w", fuse.EIO, path, err)
	}
	fi, err := file.Stat()
	file.Close()
	if err != nil {
		return fmt.Errorf("%w: %w", fuse.EIO, err)
	}
	op.Attributes = f.infoAttributes(fi)
	if f.cacheDur != 0 {
		op.AttributesExpiration = time.Now().Add(f.cacheDur)
	}
	return nil
}

func (f *FS) forgetInode(inode fuseops.InodeID, N uint64) error {
	if N == 0 {
		N = 1
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if rc, ok := f.inodeRefCounts[inode]; ok {
		if uint64(rc) > N {
			f.inodeRefCounts[inode] = rc - uint32(N)
		} else {
			delete(f.pathInodes, f.inodePaths[inode])
			delete(f.inodePaths, inode)
			delete(f.inodeRefCounts, inode)
		}
	}
	return nil
}

func (f *FS) ForgetInode(ctx context.Context, op *fuseops.ForgetInodeOp) error {
	return f.forgetInode(op.Inode, 1)
}

func (f *FS) BatchForget(ctx context.Context, op *fuseops.BatchForgetOp) error {
	var firstErr error
	for _, e := range op.Entries {
		if err := f.forgetInode(e.Inode, e.N); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (f *FS) openFile(inode fuseops.InodeID) (fuseops.HandleID, error) {
	f.mu.RLock()
	path, ok := f.inodePaths[inode]
	f.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("%d: %w", inode, fuse.ENOENT)
	}
	file, err := f.fsys.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("%w: %q: %w", fuse.ENOENT, path, err)
		}
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	handle := fuseops.HandleID(atomic.AddUint64(&f.handleSeq, 1))
	f.files[handle] = file
	return handle, nil
}

func (f *FS) OpenDir(ctx context.Context, op *fuseops.OpenDirOp) error {
	handle, err := f.openFile(op.Inode)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fuse.ENOENT
		}
		return err
	}
	op.Handle = handle
	return nil
}

func (f *FS) ReadDir(ctx context.Context, op *fuseops.ReadDirOp) error {
	f.mu.RLock()
	dn := f.inodePaths[op.Inode]
	f.mu.RUnlock()
	dis, err := fs.ReadDir(f.fsys, dn)
	if 0 < op.Offset && op.Offset <= fuseops.DirOffset(len(dis)) {
		dis = dis[op.Offset:]
	}
	op.BytesRead = 0
	for i, di := range dis {
		inode := f.getPathInode(path.Join(dn, di.Name()))
		typ := fuseutil.DT_File
		if t := di.Type() & fs.ModeType; t != 0 {
			if t&fs.ModeDir != 0 || t.IsDir() {
				typ = fuseutil.DT_Directory
			} else if t&fs.ModeCharDevice != 0 {
				typ = fuseutil.DT_Char
			} else if t&fs.ModeDevice != 0 {
				typ = fuseutil.DT_Block
			} else if t&fs.ModeNamedPipe != 0 {
				typ = fuseutil.DT_FIFO
			}
		}
		n := fuseutil.WriteDirent(op.Dst[op.BytesRead:], fuseutil.Dirent{
			Offset: op.Offset + fuseops.DirOffset(i+1),
			Inode:  inode,
			Name:   di.Name(),
			Type:   typ,
		})
		//log.Println("off:", op.Offset+fuseops.DirOffset(i+1), "bytesRead:", op.BytesRead, "dirents:", n)
		op.BytesRead += n
		if n == 0 {
			break
		}
	}
	return err
}
func (f *FS) ReleaseDirHandle(ctx context.Context, op *fuseops.ReleaseDirHandleOp) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if file, ok := f.files[op.Handle]; ok {
		file.Close()
		delete(f.files, op.Handle)
	}
	return nil
}

func (f *FS) OpenFile(ctx context.Context, op *fuseops.OpenFileOp) error {
	file, err := f.openFile(op.Inode)
	if err != nil {
		return err
	}
	op.Handle = file
	op.KeepPageCache = true
	return err
}

func (f *FS) ReadFile(ctx context.Context, op *fuseops.ReadFileOp) error {
	f.mu.RLock()
	file, ok := f.files[op.Handle]
	path := f.inodePaths[op.Inode]
	f.mu.RUnlock()
	if !ok {
		return fuse.EINVAL
	}
	var err error
	if spec, ok := file.(io.ReaderAt); ok {
		op.BytesRead, err = spec.ReadAt(op.Dst, op.Offset)
	} else if spec, ok := file.(io.Seeker); ok {
		if _, err = spec.Seek(op.Offset, io.SeekStart); err != nil {
			return err
		}
		op.BytesRead, err = file.Read(op.Dst)
	} else if spec, ok := f.fsys.(fs.ReadFileFS); ok {
		var data []byte
		data, err = spec.ReadFile(path)
		op.BytesRead = copy(op.Dst, data[op.Offset:])
	} else {
		file, openErr := f.fsys.Open(path)
		if openErr != nil {
			return openErr
		}
		defer file.Close()
		if op.Offset != 0 {
			if _, err = io.CopyBuffer(io.Discard, io.LimitReader(file, op.Offset), op.Dst); err != nil {
				return err
			}
		}
		op.BytesRead, err = file.Read(op.Dst)
	}

	// Don't return EOF errors; we just indicate EOF to fuse using a short read.
	if err == io.EOF {
		return nil
	}
	return err
}

func (f *FS) ReleaseFileHandle(ctx context.Context, op *fuseops.ReleaseFileHandleOp) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if file, ok := f.files[op.Handle]; ok {
		file.Close()
		delete(f.files, op.Handle)
	}
	return nil
}

func (f *FS) FlushFile(context.Context, *fuseops.FlushFileOp) error { return nil }
