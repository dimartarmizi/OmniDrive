//go:build windows
// +build windows

package vfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/oauth2"

	"omnidrive/config"
	"omnidrive/database"
	"omnidrive/driveapi"
)

type Runtime struct {
	store        *database.Store
	oauthConfig  *oauth2.Config
	cacheDir     string
	stagingDir   string
	handleSeq    atomic.Uint64
	handlesMu    sync.Mutex
	handles      map[uint64]*fileHandle
	downloadsMu  sync.Mutex
	downloadOnce map[string]*sync.Once
	host         *fuse.FileSystemHost
	syncStart    sync.Once
	syncMu       sync.Mutex
	syncCond     *sync.Cond
	syncRunning  bool
	lastSync     time.Time
	lastSyncErr  error
	syncInterval time.Duration
	dirMaxAge    time.Duration
}

type fileHandle struct {
	id          uint64
	virtualPath string
	localPath   string
	file        *os.File
	meta        database.VirtualFile
	dirty       bool
	isNew       bool
	modified    bool
	deleted     bool
}

func NewRuntime(store *database.Store, oauthConfig *oauth2.Config) *Runtime {
	rt := &Runtime{
		store:        store,
		oauthConfig:  oauthConfig,
		cacheDir:     config.DefaultCacheDir(),
		stagingDir:   config.DefaultStagingDir(),
		handles:      map[uint64]*fileHandle{},
		downloadOnce: map[string]*sync.Once{},
		syncInterval: 10 * time.Second,
		dirMaxAge:    5 * time.Second,
	}
	rt.syncCond = sync.NewCond(&rt.syncMu)
	return rt
}

func (rt *Runtime) attachHost(host *fuse.FileSystemHost) {
	rt.syncMu.Lock()
	rt.host = host
	rt.syncMu.Unlock()
}

func (rt *Runtime) ensureDirs() error {
	if err := os.MkdirAll(rt.cacheDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(rt.stagingDir, 0o700); err != nil {
		return err
	}
	return nil
}

func (rt *Runtime) nextHandleID() uint64 {
	return rt.handleSeq.Add(1)
}

func (rt *Runtime) cachePathFor(virtualPath string) string {
	return filepath.Join(rt.cacheDir, filepath.FromSlash(virtualPath))
}

func (rt *Runtime) stagePathFor(virtualPath string) string {
	return filepath.Join(rt.stagingDir, filepath.FromSlash(virtualPath))
}

func (rt *Runtime) openClient(ctx context.Context, email string) (*driveapi.Client, error) {
	account, err := rt.store.GetAccount(ctx, email)
	if err != nil {
		return nil, err
	}
	refreshToken, err := config.UnprotectString(account.EncryptedRefreshToken)
	if err != nil {
		return nil, fmt.Errorf("decrypt refresh token for %s: %w", email, err)
	}
	token := &oauth2.Token{RefreshToken: refreshToken}
	return driveapi.NewClient(ctx, rt.oauthConfig, token, email)
}

func (rt *Runtime) rememberHandle(h *fileHandle) uint64 {
	rt.handlesMu.Lock()
	defer rt.handlesMu.Unlock()
	rt.handles[h.id] = h
	return h.id
}

func (rt *Runtime) handle(id uint64) *fileHandle {
	rt.handlesMu.Lock()
	defer rt.handlesMu.Unlock()
	return rt.handles[id]
}

func (rt *Runtime) closeHandle(id uint64) {
	rt.handlesMu.Lock()
	h := rt.handles[id]
	delete(rt.handles, id)
	rt.handlesMu.Unlock()
	if h != nil && h.file != nil {
		_ = h.file.Close()
	}
}

func (rt *Runtime) startBackgroundSync() {
	rt.syncStart.Do(func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			_ = rt.refreshNow(ctx, 0)
			cancel()
			ticker := time.NewTicker(rt.syncInterval)
			defer ticker.Stop()
			for range ticker.C {
				rt.refreshIfStale(rt.syncInterval)
			}
		}()
	})
}

func (rt *Runtime) refreshIfStale(maxAge time.Duration) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = rt.refreshNow(ctx, maxAge)
	}()
}

func (rt *Runtime) refreshDirectoriesNow(ctx context.Context) error {
	return rt.refreshNow(ctx, rt.dirMaxAge)
}

func (rt *Runtime) refreshNow(ctx context.Context, maxAge time.Duration) error {
	rt.syncMu.Lock()
	for rt.syncRunning {
		if maxAge > 0 && !rt.lastSync.IsZero() && time.Since(rt.lastSync) < maxAge {
			rt.syncMu.Unlock()
			return nil
		}
		rt.syncCond.Wait()
	}
	if maxAge > 0 && !rt.lastSync.IsZero() && time.Since(rt.lastSync) < maxAge {
		rt.syncMu.Unlock()
		return nil
	}
	rt.syncRunning = true
	rt.syncMu.Unlock()

	err := rt.syncAllAccounts(ctx)

	rt.syncMu.Lock()
	if err == nil {
		rt.lastSync = time.Now()
	}
	rt.lastSyncErr = err
	rt.syncRunning = false
	rt.syncCond.Broadcast()
	rt.syncMu.Unlock()
	return err
}

func (rt *Runtime) syncAllAccounts(ctx context.Context) error {
	before, err := rt.store.ListVirtualFiles(ctx)
	if err != nil {
		return err
	}
	accounts, err := rt.store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return nil
	}

	var allFiles []database.VirtualFile
	for _, account := range accounts {
		client, err := rt.openClient(ctx, account.Email)
		if err != nil {
			return err
		}
		total, used, email, err := client.About(ctx)
		if err != nil {
			return err
		}
		if email == "" {
			email = account.Email
		}
		files, err := client.ListAllFiles(ctx)
		if err != nil {
			return err
		}
		for i := range files {
			files[i].AccountEmail = email
		}
		if err := rt.store.UpsertAccount(ctx, database.CloudAccount{
			Email:                 email,
			EncryptedRefreshToken: account.EncryptedRefreshToken,
			TotalSpace:            total,
			UsedSpace:             used,
			IsActive:              account.IsActive,
			AddedAt:               account.AddedAt,
		}); err != nil {
			return err
		}
		allFiles = append(allFiles, files...)
	}

	if err := rt.store.ReplaceAllFiles(ctx, driveapi.ResolveGlobalConflicts(allFiles)); err != nil {
		return err
	}

	pending := rt.pendingHandleMetadata()
	for _, meta := range pending {
		if err := rt.store.UpsertVirtualFile(ctx, meta); err != nil {
			return err
		}
	}
	after, err := rt.store.ListVirtualFiles(ctx)
	if err != nil {
		return err
	}
	rt.notifyDiff(before, after)
	return nil
}

func (rt *Runtime) pendingHandleMetadata() []database.VirtualFile {
	rt.handlesMu.Lock()
	defer rt.handlesMu.Unlock()
	metas := make([]database.VirtualFile, 0, len(rt.handles))
	for _, h := range rt.handles {
		if h == nil || h.deleted {
			continue
		}
		if h.file != nil {
			if info, err := h.file.Stat(); err == nil && !h.meta.IsDir {
				h.meta.Size = info.Size()
				h.meta.ModTime = info.ModTime()
			}
		}
		metas = append(metas, h.meta)
	}
	return metas
}

func (rt *Runtime) notifyDiff(before, after []database.VirtualFile) {
	beforeMap := make(map[string]database.VirtualFile, len(before))
	afterMap := make(map[string]database.VirtualFile, len(after))
	for _, item := range before {
		beforeMap[item.VirtualPath] = item
	}
	for _, item := range after {
		afterMap[item.VirtualPath] = item
	}

	parents := map[string]bool{}
	for p, oldItem := range beforeMap {
		newItem, ok := afterMap[p]
		if !ok {
			rt.notifyPath(p, notifyDeleteAction(oldItem))
			parents[parentVirtualPath(p)] = true
			continue
		}
		if oldItem.Size != newItem.Size {
			rt.notifyPath(p, fuse.NOTIFY_TRUNCATE|fuse.NOTIFY_UTIME)
			parents[parentVirtualPath(p)] = true
			continue
		}
		if !oldItem.ModTime.Equal(newItem.ModTime) || oldItem.DisplayName != newItem.DisplayName {
			rt.notifyPath(p, fuse.NOTIFY_UTIME)
			parents[parentVirtualPath(p)] = true
		}
	}
	for p, newItem := range afterMap {
		if _, ok := beforeMap[p]; ok {
			continue
		}
		rt.notifyPath(p, notifyCreateAction(newItem))
		parents[parentVirtualPath(p)] = true
	}
	for parent := range parents {
		rt.notifyPath(parent, fuse.NOTIFY_UTIME)
	}
}

func (rt *Runtime) notifyPath(virtualPath string, action uint32) {
	rt.syncMu.Lock()
	host := rt.host
	rt.syncMu.Unlock()
	if host == nil || action == 0 {
		return
	}
	path := "/"
	if virtualPath != "" {
		path = "/" + strings.TrimPrefix(virtualPath, "/")
	}
	_ = host.Notify(path, action)
}

func notifyCreateAction(meta database.VirtualFile) uint32 {
	if meta.IsDir {
		return fuse.NOTIFY_MKDIR
	}
	return fuse.NOTIFY_CREATE
}

func notifyDeleteAction(meta database.VirtualFile) uint32 {
	if meta.IsDir {
		return fuse.NOTIFY_RMDIR
	}
	return fuse.NOTIFY_UNLINK
}
