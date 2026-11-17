package store

import "time"

type BenchmarkRun struct {
	ID           uint   `gorm:"primaryKey"`
	Name         string `gorm:"size:128"`
	TargetKind   string `gorm:"size:16;index"`
	DeploymentID uint   `gorm:"index"`
	InstanceUUID string `gorm:"size:64;index"`
	BaseURL      string `gorm:"size:512"`
	APIKey       string `gorm:"type:text"` // ciphertext of the target's API key (EncryptAIKey)
	Model        string `gorm:"size:255"`
	LoadOrigin   string `gorm:"size:128;index"`
	// PriorityClass is the request priority class the run asked the gateway
	// for (a server-origin run of a managed deployment); empty asks for
	// none. A name, so the report still says it once the class is deleted.
	PriorityClass  string `gorm:"size:64;not null;default:''"`
	NumGPUs        int
	Dataset        string `gorm:"type:text"` // JSON dataset.Config (pkg/bench/dataset)
	Mode           string `gorm:"size:16;index"`
	Search         string `gorm:"type:text"` // JSON search.Document (pkg/bench/search)
	TokenizerRef   string `gorm:"size:512"`
	TargetSnapshot string `gorm:"type:text"` // JSON api.benchmarkTargetSnapshot
	Warnings       string `gorm:"type:text"` // JSON []string
	KeepDetail     bool
	DetailCapMB    int
	State          string `gorm:"size:16;index"`
	Progress       string `gorm:"type:text"` // JSON proto.BenchmarkStatus, the latest report
	Verdict        string `gorm:"type:text"` // JSON store.BenchmarkVerdict
	Error          string `gorm:"type:text"` // free text
	CreatedBy      uint   `gorm:"index"`
	StartedAt      *time.Time
	EndedAt        *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type BenchmarkPoint struct {
	ID          uint `gorm:"primaryKey"`
	RunID       uint `gorm:"index:idx_benchmark_point,unique,priority:1"`
	Seq         int  `gorm:"index:idx_benchmark_point,unique,priority:2"`
	Concurrency int
	TargetISL   int
	TargetOSL   int
	// RequestRate is the open-loop arrival rate of the point in req/s; 0 is a
	// closed-loop point. In open loop Concurrency is the in-flight cap.
	RequestRate   float64 `gorm:"not null;default:0"`
	Repeat        int
	State         string `gorm:"size:16;index"`
	ActualISL     float64
	ActualOSL     float64
	InputTokens   int64
	OutputTokens  int64
	Success       int
	Errors        int
	OutputTput    float64
	TotalTput     float64
	MaxOutputTput float64
	MaxTotalTput  float64
	RequestTput   float64
	AvgTTFT       float64
	P50TTFT       float64
	P99TTFT       float64
	AvgTPOT       float64
	P50TPOT       float64
	P99TPOT       float64
	AvgLatency    float64
	P50Latency    float64
	P99Latency    float64
	LongTail      string `gorm:"type:text"` // JSON benchrun.LongTailDoc (tail percentiles, cache and speculative facts)
	Curve         string `gorm:"type:text"` // JSON []store.BenchmarkCurveSample
	DetailPath    string `gorm:"size:512"`
	StartedAt     *time.Time
	EndedAt       *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
