package main

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
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	cmd := os.Args[1]
	args := os.Args[2:]
	opt := parseOptions(cmd, args)

	store, err := database.Open(ctx, opt.dbPath)
	fatalIf(err)
	defer store.Close()

	switch cmd {
	case "init":
		fmt.Println("OmniDrive database ready:", opt.dbPath)
	case "add-account":
		fatalIf(requireCredentials(opt.credentialsPath))
		fatalIf(addAccount(ctx, store, opt.credentialsPath))
	case "sync":
		fatalIf(requireCredentials(opt.credentialsPath))
		fatalIf(syncAccounts(ctx, store, opt.credentialsPath))
	case "list":
		fatalIf(listFiles(ctx, store))
	case "best-account":
		fatalIf(bestAccount(ctx, store))
	case "mount":
		fatalIf(requireCredentials(opt.credentialsPath))
		oauthConfig, err := driveapi.LoadOAuthConfig(opt.credentialsPath)
		fatalIf(err)
		code := vfs.Mount(store, oauthConfig, opt.mountPoint)
		os.Exit(code)
	default:
		usage()
		os.Exit(2)
	}
}

func parseOptions(cmd string, args []string) options {
	opt := options{dbPath: config.DefaultDBPath(), mountPoint: vfs.DefaultMountPoint}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.StringVar(&opt.dbPath, "db", opt.dbPath, "SQLite database path")
	fs.StringVar(&opt.credentialsPath, "credentials", "credentials.json", "Google OAuth credentials JSON")
	fs.StringVar(&opt.mountPoint, "mountpoint", opt.mountPoint, "VFS mount point")
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
		if err := store.UpsertAccount(ctx, database.CloudAccount{Email: email, EncryptedRefreshToken: account.EncryptedRefreshToken, TotalSpace: total, UsedSpace: used, IsActive: true, AddedAt: account.AddedAt}); err != nil {
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
	fmt.Printf("%s free=%d bytes\n", account.Email, account.TotalSpace-account.UsedSpace)
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
	fmt.Println("  sync --credentials credentials.json [--db path]")
	fmt.Println("  list [--db path]")
	fmt.Println("  best-account [--db path]")
	fmt.Println("  mount [--mountpoint Z:] [--db path]")
}

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
