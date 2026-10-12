package config

// DMPolicy is independent of channel retention. Zero retention means keep until
// explicitly deleted; storage and transfer budgets still apply.
type DMPolicy struct {
	Version             int   `json:"version"`
	ImagesEnabled       bool  `json:"images_enabled"`
	ImageMaxBytes       int64 `json:"image_max_bytes"`
	UserStorageBytes    int64 `json:"user_storage_bytes"`
	StorageBytes        int64 `json:"storage_bytes"`
	TextMaxBytes        int64 `json:"text_max_bytes"`
	TextRetentionDays   int64 `json:"text_retention_days"`
	ImageRetentionDays  int64 `json:"image_retention_days"`
	UploadBytesPerDay   int64 `json:"upload_bytes_per_day"`
	DownloadBytesPerDay int64 `json:"download_bytes_per_day"`
}

func DefaultDMPolicy() DMPolicy {
	return DMPolicy{Version: 1, ImagesEnabled: true, ImageMaxBytes: 2 << 20, UserStorageBytes: 100 << 20, StorageBytes: 1 << 30, TextMaxBytes: 256 << 20, UploadBytesPerDay: 100 << 20, DownloadBytesPerDay: 1 << 30}
}
func (p Policy) WithDefaults() Policy {
	if p.DM.Version == 0 {
		p.DM = DefaultDMPolicy()
	}
	return p
}
func (p DMPolicy) Valid() bool {
	if p.Version != 1 || p.ImageMaxBytes < 1024 || p.ImageMaxBytes > 20<<20 || p.UserStorageBytes < p.ImageMaxBytes || p.StorageBytes < p.ImageMaxBytes || p.TextMaxBytes < 1<<20 || p.TextRetentionDays < 0 || p.TextRetentionDays > 36500 || p.ImageRetentionDays < 0 || p.ImageRetentionDays > 36500 {
		return false
	}
	for _, v := range []int64{p.UserStorageBytes, p.StorageBytes, p.TextMaxBytes, p.UploadBytesPerDay, p.DownloadBytesPerDay} {
		if v < 1024 || v > 1<<50 {
			return false
		}
	}
	return true
}
