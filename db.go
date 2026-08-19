package main

import (
	"fmt"
	"log"
	"time"

	"github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const maxMediumBlob = 1<<24 - 1 // MEDIUMBLOB
const maxErrorLen = 1024

type CallLogger interface {
	Record(CallRecord)
}

type CallRecord struct {
	Model          string
	Provider       string
	ProviderModel  string
	APIKeyName     string
	InputTokens    int64
	OutputTokens   int64
	CacheTokens    int64
	UncachedTokens int64
	HTTPStatus     int
	Error          string
	RequestJSON    []byte
	ResponseJSON   []byte
}

type LLMCall struct {
	ID             uint64    `gorm:"primaryKey"`
	CreatedAt      time.Time `gorm:"type:datetime(3);index"`
	Model          string    `gorm:"size:255;not null"`
	Provider       string    `gorm:"size:255;not null"`
	ProviderModel  string    `gorm:"size:255"`
	APIKeyName     string    `gorm:"size:255"`
	InputTokens    int64
	OutputTokens   int64
	CacheTokens    int64
	UncachedTokens int64
	HTTPStatus     int
	Error          string `gorm:"size:1024"`
	RequestJSON    []byte `gorm:"type:mediumblob"`
	ResponseJSON   []byte `gorm:"type:mediumblob"`
}

func (LLMCall) TableName() string {
	return "llm_calls"
}

func normalizeMySQLDSN(dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", err
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	if err := cfg.Apply(mysql.Charset("utf8mb4", "")); err != nil {
		return "", err
	}
	if cfg.Params == nil {
		cfg.Params = make(map[string]string)
	}
	delete(cfg.Params, "charset")
	cfg.Params["time_zone"] = "'UTC'"
	return cfg.FormatDSN(), nil
}

func openDB(cfg MySQLConfig) (*gorm.DB, func() error, error) {
	dsn, err := normalizeMySQLDSN(cfg.DSN)
	if err != nil {
		return nil, nil, fmt.Errorf("mysql dsn: %w", err)
	}
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{
		NowFunc: func() time.Time { return time.Now().UTC() },
		Logger:  logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, nil, err
	}
	if err := db.AutoMigrate(&LLMCall{}); err != nil {
		sqlDB, _ := db.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
		return nil, nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	return db, sqlDB.Close, nil
}

type gormCallLogger struct {
	db     *gorm.DB
	retain int
}

func newGormCallLogger(db *gorm.DB, retain int) *gormCallLogger {
	return &gormCallLogger{db: db, retain: retain}
}

func (l *gormCallLogger) Record(rec CallRecord) {
	if l == nil || l.db == nil {
		return
	}
	call := LLMCall{
		Model:          rec.Model,
		Provider:       rec.Provider,
		ProviderModel:  rec.ProviderModel,
		APIKeyName:     rec.APIKeyName,
		InputTokens:    rec.InputTokens,
		OutputTokens:   rec.OutputTokens,
		CacheTokens:    rec.CacheTokens,
		UncachedTokens: rec.UncachedTokens,
		HTTPStatus:     rec.HTTPStatus,
		Error:          clipError(rec.Error),
		RequestJSON:    clipBlob(rec.RequestJSON),
		ResponseJSON:   clipBlob(rec.ResponseJSON),
	}
	if l.retain <= 0 {
		call.RequestJSON = nil
		call.ResponseJSON = nil
	}
	if err := l.db.Create(&call).Error; err != nil {
		log.Println("llm_calls insert:", err)
		return
	}
	if err := l.prune(); err != nil {
		log.Println("llm_calls prune:", err)
	}
}

func (l *gormCallLogger) prune() error {
	if l.retain <= 0 {
		return l.db.Model(&LLMCall{}).
			Where("request_json IS NOT NULL OR response_json IS NOT NULL").
			UpdateColumns(map[string]any{
				"request_json":  gorm.Expr("NULL"),
				"response_json": gorm.Expr("NULL"),
			}).Error
	}
	var cutoff uint64
	if err := l.db.Model(&LLMCall{}).Select("id").Order("id DESC").Offset(l.retain - 1).Limit(1).Scan(&cutoff).Error; err != nil {
		return err
	}
	if cutoff == 0 {
		return nil
	}
	return l.db.Model(&LLMCall{}).
		Where("id < ? AND (request_json IS NOT NULL OR response_json IS NOT NULL)", cutoff).
		UpdateColumns(map[string]any{
			"request_json":  gorm.Expr("NULL"),
			"response_json": gorm.Expr("NULL"),
		}).Error
}

func clipBlob(b []byte) []byte {
	if len(b) == 0 || len(b) > maxMediumBlob {
		return nil
	}
	return b
}

func clipError(s string) string {
	if len(s) <= maxErrorLen {
		return s
	}
	return s[:maxErrorLen]
}
