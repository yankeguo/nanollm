package main

import (
	"fmt"
	"log"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	ID             uint64    `gorm:"column:id;type:bigint unsigned;primaryKey;autoIncrement"`
	CreatedAt      time.Time `gorm:"column:created_at;type:datetime(3);not null;index"`
	Model          string    `gorm:"column:model;type:varchar(255);not null;index"`
	Provider       string    `gorm:"column:provider;type:varchar(255);not null;index"`
	ProviderModel  string    `gorm:"column:provider_model;type:varchar(255);not null"`
	APIKeyName     string    `gorm:"column:api_key_name;type:varchar(255);not null;index"`
	InputTokens    int64     `gorm:"column:input_tokens;type:bigint;not null"`
	OutputTokens   int64     `gorm:"column:output_tokens;type:bigint;not null"`
	CacheTokens    int64     `gorm:"column:cache_tokens;type:bigint;not null"`
	UncachedTokens int64     `gorm:"column:uncached_tokens;type:bigint;not null"`
	HTTPStatus     int       `gorm:"column:http_status;type:int;not null"`
	Error          string    `gorm:"column:error;type:varchar(1024);not null"`
	RequestJSON    []byte    `gorm:"column:request_json;type:mediumblob"`
	ResponseJSON   []byte    `gorm:"column:response_json;type:mediumblob"`
}

func (LLMCall) TableName() string {
	return "llm_calls"
}

type LLMFile struct {
	SHA256    string    `gorm:"column:sha256;type:varchar(64);primaryKey"`
	MimeType  string    `gorm:"column:mime_type;type:varchar(128);not null"`
	Size      int       `gorm:"column:size;type:int;not null"`
	Data      []byte    `gorm:"column:data;type:mediumblob;not null"`
	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null"`
	VisitedAt time.Time `gorm:"column:visited_at;type:datetime(3);not null;index"`
}

func (LLMFile) TableName() string {
	return "llm_files"
}

type LLMCallFile struct {
	CallID uint64 `gorm:"column:call_id;type:bigint unsigned;primaryKey"`
	SHA256 string `gorm:"column:sha256;type:varchar(64);primaryKey;index:idx_llm_call_files_sha256"`
	Seq    int    `gorm:"column:seq;type:int;not null"`
}

func (LLMCallFile) TableName() string {
	return "llm_call_files"
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
	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	sqlDB.SetMaxOpenConns(32)
	sqlDB.SetMaxIdleConns(8)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	if err := db.AutoMigrate(&LLMCall{}, &LLMFile{}, &LLMCallFile{}); err != nil {
		_ = sqlDB.Close()
		return nil, nil, err
	}
	return db, sqlDB.Close, nil
}

type gormCallLogger struct {
	db      *gorm.DB
	retain  time.Duration
	pruning sync.Mutex
	// sincePrune counts inserts since the last prune; pruneAsync only runs
	// every pruneEveryN inserts to avoid one UPDATE per request.
	sincePrune int
	// pruneFn, if set, replaces prune (tests).
	pruneFn func() error
}

const pruneEveryN = 50

func newGormCallLogger(db *gorm.DB, retain time.Duration) *gormCallLogger {
	return &gormCallLogger{db: db, retain: retain}
}

func (l *gormCallLogger) Record(rec CallRecord) {
	if l == nil || l.db == nil {
		return
	}
	req, resp := rec.RequestJSON, rec.ResponseJSON
	var files []extractedFile
	if l.retain > 0 {
		var reqFiles, respFiles []extractedFile
		if retainableBlob(req) {
			req, reqFiles = extractFiles(req)
		}
		if retainableBlob(resp) {
			resp, respFiles = extractFiles(resp)
		}
		files = mergeExtracted(reqFiles, respFiles)
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
		RequestJSON:    clipBlob(req),
		ResponseJSON:   clipBlob(resp),
	}
	if l.retain <= 0 {
		call.RequestJSON = nil
		call.ResponseJSON = nil
	}
	if err := l.db.Create(&call).Error; err != nil {
		log.Println("llm_calls insert:", err)
		return
	}
	if err := l.storeFiles(call.ID, files); err != nil {
		log.Println("llm_files insert:", err)
	}
	go l.pruneAsync()
}

func (l *gormCallLogger) storeFiles(callID uint64, files []extractedFile) error {
	if len(files) == 0 {
		return nil
	}
	now := time.Now().UTC()
	rows := make([]LLMFile, len(files))
	joins := make([]LLMCallFile, len(files))
	for i, f := range files {
		rows[i] = LLMFile{
			SHA256:    f.SHA256,
			MimeType:  f.MimeType,
			Size:      len(f.Data),
			Data:      f.Data,
			CreatedAt: now,
			VisitedAt: now,
		}
		joins[i] = LLMCallFile{CallID: callID, SHA256: f.SHA256, Seq: i}
	}
	// Touch visited_at on reuse so prune's retain cutoff covers the window
	// between this INSERT and the join row, without rewriting created_at.
	if err := l.db.Clauses(clause.OnConflict{
		DoUpdates: clause.Assignments(map[string]any{"visited_at": now}),
	}).Create(&rows).Error; err != nil {
		return err
	}
	return l.db.Create(&joins).Error
}

func (l *gormCallLogger) pruneAsync() {
	if !l.pruning.TryLock() {
		return
	}
	l.sincePrune++
	if l.sincePrune < pruneEveryN {
		l.pruning.Unlock()
		return
	}
	l.sincePrune = 0
	defer l.pruning.Unlock()
	if err := l.runPrune(); err != nil {
		log.Println("llm_calls prune:", err)
	}
}

func (l *gormCallLogger) runPrune() error {
	if l.pruneFn != nil {
		return l.pruneFn()
	}
	return l.prune()
}

func (l *gormCallLogger) prune() error {
	cutoff, all := pruneFileScope(l.retain, time.Now())
	q := l.db.Model(&LLMCall{}).Where("request_json IS NOT NULL OR response_json IS NOT NULL")
	if !all {
		q = q.Where("created_at < ?", cutoff)
	}
	if err := q.UpdateColumns(map[string]any{
		"request_json":  gorm.Expr("NULL"),
		"response_json": gorm.Expr("NULL"),
	}).Error; err != nil {
		return err
	}
	return l.pruneFiles(cutoff, all)
}

// pruneFileScope says how blobs and files should be cleared during prune:
// everything when retain is disabled, otherwise rows older than now-retain.
func pruneFileScope(retain time.Duration, now time.Time) (cutoff time.Time, all bool) {
	if retain <= 0 {
		return time.Time{}, true
	}
	return now.UTC().Add(-retain), false
}

func (l *gormCallLogger) pruneFiles(cutoff time.Time, all bool) error {
	q := l.db
	if all {
		q = q.Where("1 = 1")
	} else {
		q = q.Where("call_id IN (?)", l.db.Model(&LLMCall{}).Select("id").Where("created_at < ?", cutoff))
	}
	if err := q.Delete(&LLMCallFile{}).Error; err != nil {
		return err
	}
	// Unreferenced files last visited inside the retain window are kept: a
	// concurrent Record may have inserted (or touched visited_at) before
	// writing llm_call_files. The same cutoff as blob prune makes that race
	// harmless without a separate grace period.
	fq := l.db.Where("sha256 NOT IN (?)", l.db.Model(&LLMCallFile{}).Select("sha256"))
	if !all {
		fq = fq.Where("visited_at < ?", cutoff)
	}
	return fq.Delete(&LLMFile{}).Error
}

// retainableBlob reports whether a detail JSON blob survives clipBlob; only
// then is extracting files from it worthwhile.
func retainableBlob(b []byte) bool {
	return len(b) > 0 && len(b) <= maxMediumBlob
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
	s = s[:maxErrorLen]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}
