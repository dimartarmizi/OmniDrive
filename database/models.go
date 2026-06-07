package database

import "time"

type CloudAccount struct {
	Email                 string
	EncryptedRefreshToken string
	TotalSpace            int64
	UsedSpace             int64
	Priority              int
	IsActive              bool
	AddedAt               time.Time
}

type VirtualFile struct {
	VirtualPath  string
	DisplayName  string
	OriginalName string
	IsDir        bool
	Size         int64
	ModTime      time.Time
	AccountEmail string
	GoogleFileID string
	ParentID     string
	MimeType     string
}
