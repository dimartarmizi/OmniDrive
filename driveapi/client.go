package driveapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/oauth2"
	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"omnidrive/database"
)

const folderMimeType = "application/vnd.google-apps.folder"

var exportMimeTypes = map[string]string{
	"application/vnd.google-apps.document":     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/vnd.google-apps.spreadsheet":  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/vnd.google-apps.presentation": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"application/vnd.google-apps.drawing":      "image/png",
	"application/vnd.google-apps.script":       "application/vnd.google-apps.script+json",
}

type Client struct {
	service *drive.Service
	email   string
}

func NewClient(ctx context.Context, oauthConfig *oauth2.Config, token *oauth2.Token, email string) (*Client, error) {
	hc := oauthConfig.Client(ctx, token)
	svc, err := drive.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, err
	}
	return &Client{service: svc, email: email}, nil
}

func (c *Client) About(ctx context.Context) (total, used int64, email string, err error) {
	about, err := c.service.About.Get().Fields("user(emailAddress),storageQuota(limit,usage)").Context(ctx).Do()
	if err != nil {
		return 0, 0, "", err
	}
	if about.StorageQuota != nil {
		total = about.StorageQuota.Limit
		used = about.StorageQuota.Usage
	}
	if about.User != nil {
		email = about.User.EmailAddress
	}
	return total, used, email, nil
}

func (c *Client) ListAllFiles(ctx context.Context) ([]database.VirtualFile, error) {
	var files []*drive.File
	call := c.service.Files.List().Spaces("drive").PageSize(1000).Fields("nextPageToken, files(id, name, mimeType, size, modifiedTime, parents, trashed)").Q("trashed=false")
	for {
		resp, err := call.Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		files = append(files, resp.Files...)
		if resp.NextPageToken == "" {
			break
		}
		call.PageToken(resp.NextPageToken)
	}
	return MergeDriveFiles(c.email, files), nil
}

func (c *Client) DownloadFile(ctx context.Context, file database.VirtualFile, localPath string) error {
	if file.IsDir {
		return errors.New("cannot download a directory")
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o700); err != nil {
		return err
	}

	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()

	var rc io.ReadCloser
	if exportMime, ok := exportMimeTypes[file.MimeType]; ok {
		resp, err := c.service.Files.Export(file.GoogleFileID, exportMime).Context(ctx).Download()
		if err != nil {
			return err
		}
		rc = resp.Body
		defer rc.Close()
	} else {
		resp, err := c.service.Files.Get(file.GoogleFileID).Context(ctx).Download()
		if err != nil {
			return err
		}
		rc = resp.Body
		defer rc.Close()
	}

	if _, err := io.Copy(out, rc); err != nil {
		return err
	}
	return out.Close()
}

func (c *Client) CreateFolder(ctx context.Context, name, parentID string) (*drive.File, error) {
	if parentID == "" {
		parentID = "root"
	}
	f := &drive.File{Name: name, MimeType: folderMimeType, Parents: []string{parentID}}
	return c.service.Files.Create(f).Fields("id,name,mimeType,size,modifiedTime,parents").Context(ctx).Do()
}

func (c *Client) UploadFile(ctx context.Context, name, parentID, localPath string) (*drive.File, error) {
	if parentID == "" {
		parentID = "root"
	}
	in, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer in.Close()

	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))
	file := &drive.File{Name: name, Parents: []string{parentID}}
	call := c.service.Files.Create(file).Fields("id,name,mimeType,size,modifiedTime,parents")
	if mimeType != "" {
		call = call.Media(in, googleapi.ContentType(mimeType))
	} else {
		call = call.Media(in)
	}
	return call.Context(ctx).Do()
}

func (c *Client) UpdateFileContent(ctx context.Context, googleFileID, localPath string) (*drive.File, error) {
	in, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer in.Close()

	call := c.service.Files.Update(googleFileID, &drive.File{}).Fields("id,name,mimeType,size,modifiedTime,parents")
	return call.Media(in).Context(ctx).Do()
}

func (c *Client) RenameFile(ctx context.Context, googleFileID, newName, parentID string) (*drive.File, error) {
	update := &drive.File{Name: newName}
	call := c.service.Files.Update(googleFileID, update).Fields("id,name,mimeType,size,modifiedTime,parents")
	if parentID != "" {
		meta, err := c.service.Files.Get(googleFileID).Fields("parents").Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		if len(meta.Parents) > 0 {
			call = call.RemoveParents(strings.Join(meta.Parents, ","))
		}
		call = call.AddParents(parentID)
	}
	return call.Context(ctx).Do()
}

func (c *Client) DeleteFile(ctx context.Context, googleFileID string) error {
	return c.service.Files.Delete(googleFileID).Context(ctx).Do()
}

func DriveFileToVirtualFile(email, virtualPath string, f *drive.File) database.VirtualFile {
	modTime := time.Now()
	if f.ModifiedTime != "" {
		if parsed, err := time.Parse(time.RFC3339, f.ModifiedTime); err == nil {
			modTime = parsed
		}
	}
	parent := "root"
	if len(f.Parents) > 0 {
		parent = f.Parents[0]
	}
	return database.VirtualFile{
		VirtualPath:  strings.TrimPrefix(virtualPath, "/"),
		DisplayName:  path.Base(virtualPath),
		OriginalName: f.Name,
		IsDir:        f.MimeType == folderMimeType,
		Size:         f.Size,
		ModTime:      modTime,
		AccountEmail: email,
		GoogleFileID: f.Id,
		ParentID:     parent,
		MimeType:     f.MimeType,
	}
}

func ResolveGlobalConflicts(files []database.VirtualFile) []database.VirtualFile {
	counts := map[string]int{}
	for _, f := range files {
		counts[strings.ToLower(f.VirtualPath)]++
	}
	for i := range files {
		if counts[strings.ToLower(files[i].VirtualPath)] <= 1 {
			continue
		}
		files[i].DisplayName = conflictName(files[i].OriginalName, files[i].AccountEmail)
		files[i].VirtualPath = path.Join(path.Dir(files[i].VirtualPath), files[i].DisplayName)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].VirtualPath < files[j].VirtualPath })
	return files
}

func MergeDriveFiles(email string, files []*drive.File) []database.VirtualFile {
	byID := map[string]*drive.File{}
	for _, f := range files {
		byID[f.Id] = f
	}
	var out []database.VirtualFile
	nameCounts := map[string]int{}
	for _, f := range files {
		vp := virtualPathFor(f, byID)
		nameCounts[strings.ToLower(vp)]++
	}
	for _, f := range files {
		vp := virtualPathFor(f, byID)
		displayName := f.Name
		if nameCounts[strings.ToLower(vp)] > 1 {
			displayName = conflictName(f.Name, email)
			vp = path.Join(path.Dir(vp), displayName)
		}
		modTime := time.Now()
		if f.ModifiedTime != "" {
			if parsed, err := time.Parse(time.RFC3339, f.ModifiedTime); err == nil {
				modTime = parsed
			}
		}
		parent := "root"
		if len(f.Parents) > 0 {
			parent = f.Parents[0]
		}
		out = append(out, database.VirtualFile{
			VirtualPath: strings.TrimPrefix(vp, "/"), DisplayName: displayName, OriginalName: f.Name,
			IsDir: f.MimeType == folderMimeType, Size: f.Size, ModTime: modTime,
			AccountEmail: email, GoogleFileID: f.Id, ParentID: parent, MimeType: f.MimeType,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VirtualPath < out[j].VirtualPath })
	return out
}

func virtualPathFor(f *drive.File, byID map[string]*drive.File) string {
	parts := []string{safePathPart(f.Name)}
	seen := map[string]bool{f.Id: true}
	current := f
	for len(current.Parents) > 0 {
		parentID := current.Parents[0]
		parent, ok := byID[parentID]
		if !ok || parentID == "root" || seen[parentID] {
			break
		}
		parts = append([]string{safePathPart(parent.Name)}, parts...)
		seen[parentID] = true
		current = parent
	}
	return path.Join(parts...)
}

func conflictName(name, email string) string {
	suffix := sanitizeEmail(email)
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s [%s]%s", base, suffix, ext)
}

func sanitizeEmail(email string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
	return re.ReplaceAllString(email, "_")
}

func safePathPart(name string) string {
	name = strings.ReplaceAll(name, `/`, `_`)
	name = strings.ReplaceAll(name, `\\`, `_`)
	if name == "" {
		return "unnamed"
	}
	return name
}
