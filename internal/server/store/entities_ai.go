package store

import "time"

// AIConfig is an OpenAI-compatible LLM endpoint for the AI helper (plan §14).
// The product is 100% usable without any configured — AI features simply hide.
//
// While at least one row is stored, exactly one of them is IsDefault, and that
// is the service the assistant sends to. The invariant is kept in three places
// on purpose: the store's create, update and delete keep it inside the same
// transaction as the write; migration 179 repairs a database an older build left
// broken and adds the partial unique index that stops a second mark; and the
// read path is DefaultAIConfig, which answers ErrNotFound rather than standing
// in an arbitrary row, because that fallback is exactly how the assistant ended
// up calling a service the console did not mark. The store's single writer
// connection (Open sets SetMaxOpenConns(1)) is what makes the read-then-write in
// those transactions safe on SQLite; the index is the backstop that does not
// depend on it.
type AIConfig struct {
	ID        uint   `gorm:"primaryKey"`
	Name      string `gorm:"size:64"`
	Preset    string `gorm:"size:32"`  // deepseek|bailian|siliconflow|openrouter|custom
	BaseURL   string `gorm:"size:255"` // e.g. https://api.deepseek.com/v1
	APIKey    string `gorm:"size:512"` // enc:v1:<base64>; legacy plaintext is migrated on read
	Model     string `gorm:"size:64"`
	IsDefault bool
	// ProxyMode is how calls to the service leave the server
	// (aihelper.ProxyMode): management, direct or custom. Rows stored before
	// the choice existed were migrated to management.
	ProxyMode string `gorm:"size:16"`
	// ProxyURL is the custom proxy, login included, sealed like APIKey
	// (enc:v1:<base64>); empty unless ProxyMode is custom.
	ProxyURL string `gorm:"size:1024"`
	// ImageInput is the operator's word that the model accepts images: upstream
	// models change, so it is stored rather than derived from the model name.
	ImageInput bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
