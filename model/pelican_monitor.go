package model

// PelicanTestResult survives system-task history cleanup. Only validated SVG
// is public; original upstream output is reserved for administrators.
type PelicanTestResult struct {
	TargetID         string `json:"target_id" gorm:"type:varchar(64);index"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	CacheReported    bool   `json:"cache_reported"`

	ErrorCode       string `json:"error_code,omitempty" gorm:"type:varchar(128)"`
	HTTPStatus      int    `json:"http_status,omitempty"`
	ID              int64  `json:"id" gorm:"primaryKey"`
	TaskID          string `json:"-" gorm:"type:varchar(64);index"`
	Group           string `json:"group" gorm:"type:varchar(128);index"`
	ChannelID       int    `json:"channel_id" gorm:"index"`
	ChannelName     string `json:"channel_name" gorm:"type:varchar(255)"`
	Model           string `json:"model" gorm:"type:varchar(255)"`
	StartedAt       int64  `json:"started_at" gorm:"index:idx_pelican_time"`
	FinishedAt      int64  `json:"finished_at"`
	DurationMS      int64  `json:"duration_ms"`
	Status          string `json:"status" gorm:"type:varchar(32);index"`
	Error           string `json:"error" gorm:"type:text"`
	SVG             string `json:"svg,omitempty" gorm:"type:text"`
	Output          string `json:"-" gorm:"type:text"`
	TemplateVersion string `json:"template_version" gorm:"type:varchar(32)"`
}
