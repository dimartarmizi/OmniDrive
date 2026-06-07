//go:build windows
// +build windows

package vfs

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	"omnidrive/database"
	"omnidrive/driveapi"
)

type OmniFS struct {
	fuse.FileSystemBase
	runtime *Runtime
}

const (
	fuseSIFREG uint32 = 0100000
	fuseSIFDIR uint32 = 0040000
)

func New(runtime *Runtime) *OmniFS { return &OmniFS{runtime: runtime} }

func (fs *OmniFS) Init() {
	_ = fs.runtime.ensureDirs()
	fs.runtime.startBackgroundSync()
}

func (fs *OmniFS) Destroy() {
	_ = fs.runtime.cleanupDirs()
}

func (fs *OmniFS) Getattr(name string, stat *fuse.Stat_t, fh uint64) int {
	clean := cleanFusePath(name)
	if clean == "" {
		applyDirStat(stat, time.Now())
		return 0
	}

	if h := fs.runtime.handle(fh); h != nil {
		info, err := h.file.Stat()
		if err == nil {
			if h.meta.IsDir {
				applyDirStat(stat, h.meta.ModTime)
			} else {
				applyFileStat(stat, info.Size(), info.ModTime(), true)
			}
			return 0
		}
	}

	file, err := fs.runtime.store.GetVirtualFile(context.Background(), clean)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}
	if file.IsDir {
		applyDirStat(stat, file.ModTime)
	} else {
		applyFileStat(stat, file.Size, file.ModTime, isWritable(*file))
	}
	return 0
}

func (fs *OmniFS) Readdir(name string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	_ = fs.runtime.refreshDirectoriesNow(context.Background())
	children, err := fs.runtime.store.Children(context.Background(), cleanFusePath(name))
	if err != nil {
		return -fuse.EIO
	}
	fill(".", nil, 0)
	fill("..", nil, 0)
	for _, child := range children {
		st := &fuse.Stat_t{}
		if child.IsDir {
			applyDirStat(st, child.ModTime)
		} else {
			applyFileStat(st, child.Size, child.ModTime, isWritable(child))
		}
		if !fill(path.Base(child.VirtualPath), st, 0) {
			break
		}
	}
	return 0
}

func (fs *OmniFS) Open(name string, flags int) (int, uint64) {
	ctx := context.Background()
	clean := cleanFusePath(name)
	meta, err := fs.runtime.store.GetVirtualFile(ctx, clean)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -fuse.ENOENT, 0
		}
		return -fuse.EIO, 0
	}
	if meta.IsDir {
		return -fuse.EISDIR, 0
	}

	localPath := fs.runtime.cachePathFor(clean)
	if err := fs.ensureLocalCopy(ctx, *meta, localPath); err != nil {
		return fuseErr(err), 0
	}

	file, err := os.OpenFile(localPath, openFlags(flags), 0o666)
	if err != nil {
		return fuseErr(err), 0
	}
	h := &fileHandle{id: fs.runtime.nextHandleID(), virtualPath: clean, localPath: localPath, file: file, meta: *meta}
	return 0, fs.runtime.rememberHandle(h)
}

func (fs *OmniFS) Create(name string, flags int, mode uint32) (int, uint64) {
	ctx := context.Background()
	clean := cleanFusePath(name)
	if clean == "" {
		return -fuse.EINVAL, 0
	}
	if existing, err := fs.runtime.store.GetVirtualFile(ctx, clean); err == nil {
		if existing.IsDir {
			return -fuse.EISDIR, 0
		}
		if flags&os.O_EXCL != 0 {
			return -fuse.EEXIST, 0
		}
		return fs.createOverwriteHandle(clean, flags, mode, *existing)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return -fuse.EIO, 0
	}

	parentPath := parentVirtualPath(clean)
	var parent database.VirtualFile
	if parentPath != "" {
		var err error
		parentPtr, err := fs.runtime.store.GetVirtualFile(ctx, parentPath)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return -fuse.ENOENT, 0
			}
			return -fuse.EIO, 0
		}
		parent = *parentPtr
		if !parent.IsDir {
			return -fuse.ENOTDIR, 0
		}
	}

	account, parentID, ferr := fs.pickTargetForCreate(ctx, parent)
	if ferr != 0 {
		return ferr, 0
	}
	stagePath := fs.runtime.stagePathFor(clean)
	if err := os.MkdirAll(filepath.Dir(stagePath), 0o700); err != nil {
		return fuseErr(err), 0
	}
	file, err := os.OpenFile(stagePath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(mode&0o666))
	if err != nil {
		return fuseErr(err), 0
	}
	now := time.Now()
	meta := database.VirtualFile{
		VirtualPath:  clean,
		DisplayName:  path.Base(clean),
		OriginalName: path.Base(clean),
		IsDir:        false,
		Size:         0,
		ModTime:      now,
		AccountEmail: account.Email,
		GoogleFileID: "",
		ParentID:     parentID,
		MimeType:     "application/octet-stream",
	}
	if err := fs.runtime.store.UpsertVirtualFile(ctx, meta); err != nil {
		_ = file.Close()
		return -fuse.EIO, 0
	}
	h := &fileHandle{id: fs.runtime.nextHandleID(), virtualPath: clean, localPath: stagePath, file: file, meta: meta, dirty: true, isNew: true, modified: true}
	return 0, fs.runtime.rememberHandle(h)
}

func (fs *OmniFS) createOverwriteHandle(clean string, flags int, mode uint32, meta database.VirtualFile) (int, uint64) {
	stagePath := fs.runtime.stagePathFor(clean)
	if err := os.MkdirAll(filepath.Dir(stagePath), 0o700); err != nil {
		return fuseErr(err), 0
	}
	file, err := os.OpenFile(stagePath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(mode&0o666))
	if err != nil {
		return fuseErr(err), 0
	}
	meta.ModTime = time.Now()
	meta.Size = 0
	h := &fileHandle{
		id:          fs.runtime.nextHandleID(),
		virtualPath: clean,
		localPath:   stagePath,
		file:        file,
		meta:        meta,
		dirty:       true,
		modified:    true,
	}
	if flags&os.O_APPEND != 0 {
		if _, err := file.Seek(0, io.SeekEnd); err != nil {
			_ = file.Close()
			return fuseErr(err), 0
		}
	}
	return 0, fs.runtime.rememberHandle(h)
}

func (fs *OmniFS) Read(name string, buff []byte, ofst int64, fh uint64) int {
	h := fs.runtime.handle(fh)
	if h == nil {
		return -fuse.EBADF
	}
	n, err := h.file.ReadAt(buff, ofst)
	if err != nil && !errors.Is(err, io.EOF) {
		return fuseErr(err)
	}
	return n
}

func (fs *OmniFS) Write(name string, buff []byte, ofst int64, fh uint64) int {
	h := fs.runtime.handle(fh)
	if h == nil {
		return -fuse.EBADF
	}
	n, err := h.file.WriteAt(buff, ofst)
	if err != nil {
		return fuseErr(err)
	}
	h.dirty = true
	h.modified = true
	if info, err := h.file.Stat(); err == nil {
		h.meta.Size = info.Size()
		h.meta.ModTime = info.ModTime()
	}
	return n
}

func (fs *OmniFS) Truncate(name string, size int64, fh uint64) int {
	var h *fileHandle
	if fh != ^uint64(0) {
		h = fs.runtime.handle(fh)
	}
	if h != nil {
		if err := h.file.Truncate(size); err != nil {
			return fuseErr(err)
		}
		h.dirty = true
		h.modified = true
		h.meta.Size = size
		h.meta.ModTime = time.Now()
		return 0
	}

	code, opened := fs.Open(name, os.O_RDWR)
	if code != 0 {
		return code
	}
	defer fs.Release(name, opened)
	return fs.Truncate(name, size, opened)
}

func (fs *OmniFS) Flush(name string, fh uint64) int {
	h := fs.runtime.handle(fh)
	if h == nil {
		return -fuse.EBADF
	}
	if err := h.file.Sync(); err != nil {
		return fuseErr(err)
	}
	return 0
}

func (fs *OmniFS) Release(name string, fh uint64) int {
	h := fs.runtime.handle(fh)
	if h == nil {
		return 0
	}
	defer fs.runtime.closeHandle(fh)
	if err := h.file.Sync(); err != nil {
		return fuseErr(err)
	}
	if !h.modified || h.deleted {
		return 0
	}
	if err := fs.commitHandle(context.Background(), h); err != nil {
		return fuseErr(err)
	}
	return 0
}

func (fs *OmniFS) Mkdir(name string, mode uint32) int {
	ctx := context.Background()
	clean := cleanFusePath(name)
	if clean == "" {
		return -fuse.EEXIST
	}
	if _, err := fs.runtime.store.GetVirtualFile(ctx, clean); err == nil {
		return -fuse.EEXIST
	} else if !errors.Is(err, sql.ErrNoRows) {
		return -fuse.EIO
	}

	parentPath := parentVirtualPath(clean)
	var parent database.VirtualFile
	if parentPath != "" {
		var err error
		parentPtr, err := fs.runtime.store.GetVirtualFile(ctx, parentPath)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return -fuse.ENOENT
			}
			return -fuse.EIO
		}
		parent = *parentPtr
		if !parent.IsDir {
			return -fuse.ENOTDIR
		}
	}
	account, parentID, ferr := fs.pickTargetForCreate(ctx, parent)
	if ferr != 0 {
		return ferr
	}
	client, err := fs.runtime.openClient(ctx, account.Email)
	if err != nil {
		return fuseErr(err)
	}
	created, err := client.CreateFolder(ctx, path.Base(clean), parentID)
	if err != nil {
		return fuseErr(err)
	}
	meta := driveapi.DriveFileToVirtualFile(account.Email, clean, created)
	meta.IsDir = true
	meta.DisplayName = path.Base(clean)
	if err := fs.runtime.store.UpsertVirtualFile(ctx, meta); err != nil {
		return -fuse.EIO
	}
	return fs.refreshQuota(ctx, account.Email)
}

func (fs *OmniFS) Unlink(name string) int {
	ctx := context.Background()
	clean := cleanFusePath(name)
	meta, err := fs.runtime.store.GetVirtualFile(ctx, clean)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}
	if meta.IsDir {
		return -fuse.EISDIR
	}
	client, err := fs.runtime.openClient(ctx, meta.AccountEmail)
	if err != nil {
		return fuseErr(err)
	}
	if err := client.DeleteFile(ctx, meta.GoogleFileID); err != nil {
		return fuseErr(err)
	}
	_ = os.Remove(fs.runtime.cachePathFor(clean))
	_ = os.Remove(fs.runtime.stagePathFor(clean))
	if err := fs.runtime.store.DeleteVirtualFile(ctx, clean); err != nil {
		return -fuse.EIO
	}
	return fs.refreshQuota(ctx, meta.AccountEmail)
}

func (fs *OmniFS) Rmdir(name string) int {
	ctx := context.Background()
	clean := cleanFusePath(name)
	meta, err := fs.runtime.store.GetVirtualFile(ctx, clean)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}
	if !meta.IsDir {
		return -fuse.ENOTDIR
	}
	hasChildren, err := fs.runtime.store.HasChildren(ctx, clean)
	if err != nil {
		return -fuse.EIO
	}
	if hasChildren {
		return -fuse.ENOTEMPTY
	}
	client, err := fs.runtime.openClient(ctx, meta.AccountEmail)
	if err != nil {
		return fuseErr(err)
	}
	if err := client.DeleteFile(ctx, meta.GoogleFileID); err != nil {
		return fuseErr(err)
	}
	if err := fs.runtime.store.DeleteVirtualFile(ctx, clean); err != nil {
		return -fuse.EIO
	}
	return fs.refreshQuota(ctx, meta.AccountEmail)
}

func (fs *OmniFS) Rename(oldpath string, newpath string) int {
	ctx := context.Background()
	oldClean := cleanFusePath(oldpath)
	newClean := cleanFusePath(newpath)
	if oldClean == newClean {
		return 0
	}
	meta, err := fs.runtime.store.GetVirtualFile(ctx, oldClean)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}
	if target, err := fs.runtime.store.GetVirtualFile(ctx, newClean); err == nil {
		if target.IsDir || meta.IsDir {
			return -fuse.EEXIST
		}
		if meta.AccountEmail != target.AccountEmail {
			return -fuse.EXDEV
		}
		if err := fs.replaceExistingFile(ctx, *target); err != nil {
			return fuseErr(err)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return -fuse.EIO
	}

	newParentPath := parentVirtualPath(newClean)
	parentID := ""
	if newParentPath != "" {
		parent, err := fs.runtime.store.GetVirtualFile(ctx, newParentPath)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return -fuse.ENOENT
			}
			return -fuse.EIO
		}
		if !parent.IsDir {
			return -fuse.ENOTDIR
		}
		parentID = parent.GoogleFileID
	}
	client, err := fs.runtime.openClient(ctx, meta.AccountEmail)
	if err != nil {
		return fuseErr(err)
	}
	updated, err := client.RenameFile(ctx, meta.GoogleFileID, path.Base(newClean), parentID)
	if err != nil {
		return fuseErr(err)
	}
	if err := fs.runtime.store.RenameVirtualPath(ctx, oldClean, newClean); err != nil {
		return -fuse.EIO
	}
	updatedMeta := driveapi.DriveFileToVirtualFile(meta.AccountEmail, newClean, updated)
	if meta.IsDir {
		updatedMeta.IsDir = true
	}
	if err := fs.runtime.store.UpsertVirtualFile(ctx, updatedMeta); err != nil {
		return -fuse.EIO
	}
	fs.removeLocalArtifacts(newClean)
	fs.moveLocalArtifacts(oldClean, newClean)
	return 0
}

func (fs *OmniFS) replaceExistingFile(ctx context.Context, target database.VirtualFile) error {
	client, err := fs.runtime.openClient(ctx, target.AccountEmail)
	if err != nil {
		return err
	}
	if err := client.DeleteFile(ctx, target.GoogleFileID); err != nil {
		return err
	}
	if err := fs.runtime.store.DeleteVirtualFile(ctx, target.VirtualPath); err != nil {
		return err
	}
	fs.removeLocalArtifacts(target.VirtualPath)
	return nil
}

func (fs *OmniFS) Statfs(name string, stat *fuse.Statfs_t) int {
	total, used, err := fs.runtime.store.TotalQuota(context.Background())
	if err != nil {
		return -fuse.EIO
	}
	const blockSize = uint64(4096)
	blocks := uint64(used) / blockSize
	totalBlocks := uint64(total) / blockSize
	if total%int64(blockSize) != 0 {
		totalBlocks++
	}
	if used%int64(blockSize) != 0 {
		blocks++
	}
	freeBlocks := uint64(0)
	if total > used {
		freeBlocks = uint64(total-used) / blockSize
		if (total-used)%int64(blockSize) != 0 {
			freeBlocks++
		}
	}
	stat.Bsize = blockSize
	stat.Frsize = blockSize
	stat.Blocks = totalBlocks
	stat.Bfree = freeBlocks
	stat.Bavail = freeBlocks
	stat.Files = 1_000_000
	stat.Ffree = 900_000
	stat.Favail = 900_000
	stat.Namemax = 255
	if stat.Blocks < blocks {
		stat.Blocks = blocks
	}
	return 0
}

func (fs *OmniFS) ensureLocalCopy(ctx context.Context, meta database.VirtualFile, localPath string) error {
	if info, err := os.Stat(localPath); err == nil && info.Size() == meta.Size {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o700); err != nil {
		return err
	}
	client, err := fs.runtime.openClient(ctx, meta.AccountEmail)
	if err != nil {
		return err
	}
	return client.DownloadFile(ctx, meta, localPath)
}

func (fs *OmniFS) pickTargetForCreate(ctx context.Context, parent database.VirtualFile) (database.CloudAccount, string, int) {
	if parent.AccountEmail != "" {
		account, err := fs.runtime.store.GetAccount(ctx, parent.AccountEmail)
		if err != nil {
			return database.CloudAccount{}, "", -fuse.EIO
		}
		parentID := parent.GoogleFileID
		if parentID == "" {
			parentID = "root"
		}
		return *account, parentID, 0
	}
	account, err := fs.runtime.store.BestUploadAccountForSize(ctx, 0)
	if err != nil {
		return database.CloudAccount{}, "", -fuse.ENOSPC
	}
	return *account, "root", 0
}

func (fs *OmniFS) commitHandle(ctx context.Context, h *fileHandle) error {
	if h.isNew && h.meta.ParentID == "root" {
		account, err := fs.runtime.store.BestUploadAccountForSize(ctx, h.meta.Size)
		if err != nil {
			return err
		}
		h.meta.AccountEmail = account.Email
	}
	client, err := fs.runtime.openClient(ctx, h.meta.AccountEmail)
	if err != nil {
		return err
	}
	var uploadedMeta database.VirtualFile
	if h.isNew || h.meta.GoogleFileID == "" {
		created, err := client.UploadFile(ctx, path.Base(h.virtualPath), h.meta.ParentID, h.localPath)
		if err != nil {
			return err
		}
		uploadedMeta = driveapi.DriveFileToVirtualFile(h.meta.AccountEmail, h.virtualPath, created)
	} else {
		updated, err := client.UpdateFileContent(ctx, h.meta.GoogleFileID, h.localPath)
		if err != nil {
			return err
		}
		uploadedMeta = driveapi.DriveFileToVirtualFile(h.meta.AccountEmail, h.virtualPath, updated)
	}
	if err := fs.runtime.store.UpsertVirtualFile(ctx, uploadedMeta); err != nil {
		return err
	}
	if err := fs.refreshQuota(ctx, h.meta.AccountEmail); err != 0 {
		return syscall.Errno(-err)
	}
	cachePath := fs.runtime.cachePathFor(h.virtualPath)
	if h.localPath != cachePath {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
			return err
		}
		_ = os.Remove(cachePath)
		if err := copyFile(h.localPath, cachePath); err != nil {
			return err
		}
	}
	h.meta = uploadedMeta
	h.localPath = cachePath
	h.modified = false
	h.isNew = false
	return nil
}

func (fs *OmniFS) refreshQuota(ctx context.Context, email string) int {
	client, err := fs.runtime.openClient(ctx, email)
	if err != nil {
		return -fuse.EIO
	}
	total, used, actualEmail, err := client.About(ctx)
	if err != nil {
		return -fuse.EIO
	}
	account, err := fs.runtime.store.GetAccount(ctx, email)
	if err != nil {
		return -fuse.EIO
	}
	if actualEmail == "" {
		actualEmail = email
	}
	returnCode := 0
	if err := fs.runtime.store.UpsertAccount(ctx, database.CloudAccount{Email: actualEmail, EncryptedRefreshToken: account.EncryptedRefreshToken, TotalSpace: total, UsedSpace: used, Priority: account.Priority, IsActive: account.IsActive, AddedAt: account.AddedAt}); err != nil {
		returnCode = -fuse.EIO
	}
	return returnCode
}

func (fs *OmniFS) moveLocalArtifacts(oldClean, newClean string) {
	for _, pair := range [][2]string{{fs.runtime.cachePathFor(oldClean), fs.runtime.cachePathFor(newClean)}, {fs.runtime.stagePathFor(oldClean), fs.runtime.stagePathFor(newClean)}} {
		if _, err := os.Stat(pair[0]); err == nil {
			_ = os.MkdirAll(filepath.Dir(pair[1]), 0o700)
			_ = os.Rename(pair[0], pair[1])
		}
	}
}

func (fs *OmniFS) removeLocalArtifacts(clean string) {
	_ = os.Remove(fs.runtime.cachePathFor(clean))
	_ = os.Remove(fs.runtime.stagePathFor(clean))
}

func cleanFusePath(name string) string {
	name = strings.ReplaceAll(name, `\\`, `/`)
	name = path.Clean("/" + strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "/")
	if name == "." {
		return ""
	}
	return strings.TrimSuffix(name, "/")
}

func parentVirtualPath(name string) string {
	if name == "" {
		return ""
	}
	parent := path.Dir(name)
	if parent == "." || parent == "/" {
		return ""
	}
	return strings.TrimPrefix(parent, "/")
}

func toTimespec(t time.Time) fuse.Timespec {
	if t.IsZero() {
		t = time.Now()
	}
	return fuse.Timespec{Sec: t.Unix(), Nsec: int64(t.Nanosecond())}
}

func applyDirStat(stat *fuse.Stat_t, modTime time.Time) {
	stat.Mode = fuseSIFDIR | 0o777
	stat.Nlink = 2
	stat.Mtim = toTimespec(modTime)
	stat.Ctim = stat.Mtim
	stat.Atim = stat.Mtim
}

func applyFileStat(stat *fuse.Stat_t, size int64, modTime time.Time, writable bool) {
	mode := uint32(0o444)
	if writable {
		mode = 0o666
	}
	stat.Mode = fuseSIFREG | mode
	stat.Nlink = 1
	stat.Size = size
	stat.Mtim = toTimespec(modTime)
	stat.Ctim = stat.Mtim
	stat.Atim = stat.Mtim
}

func openFlags(flags int) int {
	mask := flags & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR)
	if mask == 0 {
		mask = os.O_RDONLY
	}
	return mask
}

func isWritable(file database.VirtualFile) bool {
	return !file.IsDir
}

func fuseErr(err error) int {
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return -int(errno)
	}
	if errors.Is(err, os.ErrNotExist) {
		return -fuse.ENOENT
	}
	if errors.Is(err, os.ErrPermission) {
		return -fuse.EACCES
	}
	return -fuse.EIO
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
