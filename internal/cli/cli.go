package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/oauth2"

	"omnidrive/config"
	"omnidrive/database"
	"omnidrive/driveapi"
	"omnidrive/vfs"
)

type options struct {
	dbPath          string
	credentialsPath string
	mountPoint      string
	refreshInterval int
	cleanOnMount    bool
	cleanOnUnmount  bool
	autoSyncOnMount bool
	dirCacheMaxAge  int
	conflictPolicy  string
	logLevel        string
	mountLabel      string
	email           string
	position        int
}

func Run(args []string) int {
	if len(args) < 1 {
		usage()
		return 2
	}
	ctx := context.Background()
	cmd := args[0]
	opt := parseOptions(cmd, args[1:])

	if cmd == "config" {
		if err := saveConfig(opt); err != nil {
			return fatal(err)
		}
		fmt.Println("OmniDrive config saved:", config.DefaultConfigPath())
		return 0
	}

	store, err := database.Open(ctx, opt.dbPath)
	if err != nil {
		return fatal(err)
	}
	defer store.Close()

	switch cmd {
	case "init":
		settings, err := config.EnsureSettingsFile(config.Settings{
			CredentialsPath: opt.credentialsPath,
			MountPoint:      opt.mountPoint,
		})
		if err != nil {
			return fatal(err)
		}
		fmt.Println("OmniDrive database ready:", opt.dbPath)
		fmt.Println("OmniDrive config ready:", config.DefaultConfigPath())
		if settings.MountPoint != "" {
			fmt.Println("Mount point:", settings.MountPoint)
		}
		return 0
	case "add-account":
		if err := requireCredentials(opt.credentialsPath); err != nil {
			return fatal(err)
		}
		if err := addAccount(ctx, store, opt.credentialsPath); err != nil {
			return fatal(err)
		}
		return 0
	case "remove-account":
		if err := removeAccount(ctx, store, opt.email); err != nil {
			return fatal(err)
		}
		return 0
	case "accounts":
		if err := listAccounts(ctx, store); err != nil {
			return fatal(err)
		}
		return 0
	case "set-priority":
		if err := setPriority(ctx, store, opt.email, opt.position); err != nil {
			return fatal(err)
		}
		return 0
	case "sync":
		if err := requireCredentials(opt.credentialsPath); err != nil {
			return fatal(err)
		}
		if err := syncAccounts(ctx, store, opt.credentialsPath); err != nil {
			return fatal(err)
		}
		return 0
	case "list":
		if err := listFiles(ctx, store); err != nil {
			return fatal(err)
		}
		return 0
	case "best-account":
		if err := bestAccount(ctx, store); err != nil {
			return fatal(err)
		}
		return 0
	case "mount":
		if err := requireCredentials(opt.credentialsPath); err != nil {
			return fatal(err)
		}
		accounts, err := store.ListAccounts(ctx)
		if err != nil {
			return fatal(err)
		}
		if len(accounts) == 0 {
			return fatal(fmt.Errorf("no accounts are connected; run `go run omnidrive add-account` first"))
		}
		oauthConfig, err := driveapi.LoadOAuthConfig(opt.credentialsPath)
		if err != nil {
			return fatal(err)
		}
		settings := config.Settings{
			CredentialsPath:        opt.credentialsPath,
			MountPoint:             opt.mountPoint,
			RefreshIntervalSeconds: opt.refreshInterval,
			CleanCacheOnMount:      opt.cleanOnMount,
			CleanCacheOnUnmount:    opt.cleanOnUnmount,
			AutoSyncOnMount:        opt.autoSyncOnMount,
			DirCacheMaxAgeSeconds:  opt.dirCacheMaxAge,
			ConflictPolicy:         opt.conflictPolicy,
			LogLevel:               opt.logLevel,
			MountLabel:             opt.mountLabel,
		}
		return vfs.Mount(store, oauthConfig, settings)
	default:
		usage()
		return 2
	}
}

func parseOptions(cmd string, args []string) options {
	settings, _ := config.LoadSettings()
	opt := options{
		dbPath:          config.DefaultDBPath(),
		credentialsPath: firstNonEmpty(settings.CredentialsPath, "credentials.json"),
		mountPoint:      firstNonEmpty(settings.MountPoint, vfs.DefaultMountPoint),
		refreshInterval: settings.RefreshIntervalSeconds,
		cleanOnMount:    settings.CleanCacheOnMount,
		cleanOnUnmount:  settings.CleanCacheOnUnmount,
		autoSyncOnMount: settings.AutoSyncOnMount,
		dirCacheMaxAge:  settings.DirCacheMaxAgeSeconds,
		conflictPolicy:  settings.ConflictPolicy,
		logLevel:        settings.LogLevel,
		mountLabel:      settings.MountLabel,
		position:        1,
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.StringVar(&opt.dbPath, "db", opt.dbPath, "SQLite database path")
	fs.StringVar(&opt.credentialsPath, "credentials", opt.credentialsPath, "Google OAuth credentials JSON")
	fs.StringVar(&opt.mountPoint, "mountpoint", opt.mountPoint, "VFS mount point")
	fs.IntVar(&opt.refreshInterval, "refresh-interval", opt.refreshInterval, "Background refresh interval in seconds")
	fs.BoolVar(&opt.cleanOnMount, "clean-on-mount", opt.cleanOnMount, "Clean cache and staging directories during mount")
	fs.BoolVar(&opt.cleanOnUnmount, "clean-on-unmount", opt.cleanOnUnmount, "Clean cache and staging directories during unmount")
	fs.BoolVar(&opt.autoSyncOnMount, "auto-sync-on-mount", opt.autoSyncOnMount, "Run an immediate sync when mounting")
	fs.IntVar(&opt.dirCacheMaxAge, "dir-cache-max-age", opt.dirCacheMaxAge, "Directory cache max age in seconds")
	fs.StringVar(&opt.conflictPolicy, "conflict-policy", opt.conflictPolicy, "Conflict policy: overwrite, deny, rename")
	fs.StringVar(&opt.logLevel, "log-level", opt.logLevel, "Log level: debug, info, error")
	fs.StringVar(&opt.mountLabel, "mount-label", opt.mountLabel, "Mounted volume label")
	fs.StringVar(&opt.email, "email", "", "Google account email")
	fs.IntVar(&opt.position, "position", opt.position, "1-based account priority position")
	_ = fs.Parse(args)
	opt.credentialsPath = filepath.Clean(opt.credentialsPath)
	if opt.refreshInterval < 0 {
		opt.refreshInterval = 0
	}
	if opt.dirCacheMaxAge < 0 {
		opt.dirCacheMaxAge = 0
	}
	return opt
}

func saveConfig(opt options) error {
	settings := config.Settings{
		CredentialsPath:        opt.credentialsPath,
		MountPoint:             opt.mountPoint,
		RefreshIntervalSeconds: opt.refreshInterval,
		CleanCacheOnMount:      opt.cleanOnMount,
		CleanCacheOnUnmount:    opt.cleanOnUnmount,
		AutoSyncOnMount:        opt.autoSyncOnMount,
		DirCacheMaxAgeSeconds:  opt.dirCacheMaxAge,
		ConflictPolicy:         opt.conflictPolicy,
		LogLevel:               opt.logLevel,
		MountLabel:             opt.mountLabel,
	}
	return config.SaveSettings(settings)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func addAccount(ctx context.Context, store *database.Store, credentialsPath string) error {
	oauthConfig, err := driveapi.LoadOAuthConfig(credentialsPath)
	if err != nil {
		return err
	}
	token, err := driveapi.RunAddAccountFlow(ctx, oauthConfig)
	if err != nil {
		return err
	}
	if token.RefreshToken == "" {
		return fmt.Errorf("Google did not return a refresh token; remove prior consent and try again")
	}
	protected, err := config.ProtectString(token.RefreshToken)
	if err != nil {
		return err
	}

	email := driveapi.EmailFromIDToken(token)
	client, err := driveapi.NewClient(ctx, oauthConfig, token, email)
	if err != nil {
		return err
	}
	total, used, aboutEmail, err := client.About(ctx)
	if err != nil {
		return err
	}
	if aboutEmail != "" {
		email = aboutEmail
	}
	if email == "" {
		return fmt.Errorf("could not determine Google account email")
	}

	return store.UpsertAccount(ctx, database.CloudAccount{
		Email: email, EncryptedRefreshToken: protected, TotalSpace: total, UsedSpace: used,
		IsActive: true, AddedAt: time.Now(),
	})
}

func removeAccount(ctx context.Context, store *database.Store, email string) error {
	if email == "" {
		return fmt.Errorf("remove-account requires --email")
	}
	return store.RemoveAccount(ctx, email)
}

func listAccounts(ctx context.Context, store *database.Store) error {
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		free := account.TotalSpace - account.UsedSpace
		fmt.Printf("%d. %s free=%d used=%d total=%d\n", account.Priority, account.Email, free, account.UsedSpace, account.TotalSpace)
	}
	return nil
}

func setPriority(ctx context.Context, store *database.Store, email string, position int) error {
	if email == "" {
		return fmt.Errorf("set-priority requires --email")
	}
	if position < 1 {
		return fmt.Errorf("--position must be at least 1")
	}
	return store.SetAccountPriority(ctx, email, position)
}

func syncAccounts(ctx context.Context, store *database.Store, credentialsPath string) error {
	oauthConfig, err := driveapi.LoadOAuthConfig(credentialsPath)
	if err != nil {
		return err
	}
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return fmt.Errorf("no accounts; run add-account first")
	}
	var allFiles []database.VirtualFile
	for _, account := range accounts {
		refreshToken, err := config.UnprotectString(account.EncryptedRefreshToken)
		if err != nil {
			return fmt.Errorf("%s: %w", account.Email, err)
		}
		token := &oauth2.Token{RefreshToken: refreshToken}
		client, err := driveapi.NewClient(ctx, oauthConfig, token, account.Email)
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
		if err := store.UpsertAccount(ctx, database.CloudAccount{Email: email, EncryptedRefreshToken: account.EncryptedRefreshToken, TotalSpace: total, UsedSpace: used, IsActive: true, AddedAt: account.AddedAt, Priority: account.Priority}); err != nil {
			return err
		}
		allFiles = append(allFiles, files...)
		fmt.Printf("Synced %d files from %s\n", len(files), email)
	}
	return store.ReplaceAllFiles(ctx, driveapi.ResolveGlobalConflicts(allFiles))
}

func listFiles(ctx context.Context, store *database.Store) error {
	files, err := store.ListVirtualFiles(ctx)
	if err != nil {
		return err
	}
	for _, f := range files {
		kind := "FILE"
		if f.IsDir {
			kind = "DIR "
		}
		fmt.Printf("%s %12d %s (%s)\n", kind, f.Size, f.VirtualPath, f.AccountEmail)
	}
	return nil
}

func bestAccount(ctx context.Context, store *database.Store) error {
	account, err := store.BestUploadAccount(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("priority=%d %s free=%d bytes\n", account.Priority, account.Email, account.TotalSpace-account.UsedSpace)
	return nil
}

func requireCredentials(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("credentials file not found: %s", path)
	}
	return nil
}

func usage() {
	fmt.Println("OmniDrive commands:")
	fmt.Println("  config [--credentials credentials.json] [--mountpoint Z:] [--refresh-interval 10] [--clean-on-mount=true] [--clean-on-unmount=true] [--db path]")
	fmt.Println("  init [--db path]")
	fmt.Println("  add-account --credentials credentials.json [--db path]")
	fmt.Println("  remove-account --email user@gmail.com [--db path]")
	fmt.Println("  accounts [--db path]")
	fmt.Println("  set-priority --email user@gmail.com --position 1 [--db path]")
	fmt.Println("  sync --credentials credentials.json [--db path]")
	fmt.Println("  list [--db path]")
	fmt.Println("  best-account [--db path]")
	fmt.Println("  mount [--mountpoint Z:] [--refresh-interval 10] [--clean-on-mount=true] [--clean-on-unmount=true] [--db path]")
}

func fatal(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}
