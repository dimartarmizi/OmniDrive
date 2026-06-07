package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
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

	store, err := database.Open(ctx, opt.dbPath)
	if err != nil {
		return fatal(err)
	}
	defer store.Close()

	switch cmd {
	case "init":
		fmt.Println("OmniDrive database ready:", opt.dbPath)
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
		oauthConfig, err := driveapi.LoadOAuthConfig(opt.credentialsPath)
		if err != nil {
			return fatal(err)
		}
		return vfs.Mount(store, oauthConfig, opt.mountPoint)
	default:
		usage()
		return 2
	}
}

func parseOptions(cmd string, args []string) options {
	opt := options{dbPath: config.DefaultDBPath(), mountPoint: vfs.DefaultMountPoint, position: 1}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.StringVar(&opt.dbPath, "db", opt.dbPath, "SQLite database path")
	fs.StringVar(&opt.credentialsPath, "credentials", "credentials.json", "Google OAuth credentials JSON")
	fs.StringVar(&opt.mountPoint, "mountpoint", opt.mountPoint, "VFS mount point")
	fs.StringVar(&opt.email, "email", "", "Google account email")
	fs.IntVar(&opt.position, "position", opt.position, "1-based account priority position")
	_ = fs.Parse(args)
	return opt
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
	fmt.Println("  init [--db path]")
	fmt.Println("  add-account --credentials credentials.json [--db path]")
	fmt.Println("  remove-account --email user@gmail.com [--db path]")
	fmt.Println("  accounts [--db path]")
	fmt.Println("  set-priority --email user@gmail.com --position 1 [--db path]")
	fmt.Println("  sync --credentials credentials.json [--db path]")
	fmt.Println("  list [--db path]")
	fmt.Println("  best-account [--db path]")
	fmt.Println("  mount [--mountpoint Z:] [--db path]")
}

func fatal(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}
