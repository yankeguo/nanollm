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
	ID             uint64    `gorm:"primaryKey"`
	CreatedAt      time.Time `gorm:"type:datetime(3);index"`
	Model          string    `gorm:"size:255;not null;index"`
	Provider       string    `gorm:"size:255;not null;index"`
	ProviderModel  string    `gorm:"size:255"`
	APIKeyName     string    `gorm:"size:255;index"`
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

type LLMFile struct {
	SHA256    string `gorm:"primaryKey;size:64"`
	MimeType  string `gorm:"size:128"`
	Size      int
	Data      []byte    `gorm:"type:mediumblob"`
	CreatedAt time.Time `gorm:"type:datetime(3)"`
}

func (LLMFile) TableName() string {
	return "llm_files"
}

type LLMCallFile struct {
	CallID uint64 `gorm:"primaryKey"`
	SHA256 string `gorm:"primaryKey;size:64;index:idx_llm_call_files_sha256"`
	Seq    int
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
	retain  int
	pruning sync.Mutex
	// sincePrune counts inserts since the last prune; pruneAsync only runs
	// every pruneEveryN inserts to avoid one UPDATE per request.
	sincePrune int
	// pruneFn, if set, replaces prune (tests).
	pruneFn func() error
}

const pruneEveryN = 50

// fileGCMinAge is how old an unreferenced llm_files row must be before prune
// deletes it, closing the race with a concurrent Record's file/join inserts.
const fileGCMinAge = time.Minute

func newGormCallLogger(db *gorm.DB, retain int) *gormCallLogger {
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
	rows := make([]LLMFile, len(files))
	joins := make([]LLMCallFile, len(files))
	for i, f := range files {
		rows[i] = LLMFile{
			SHA256:   f.SHA256,
			MimeType: f.MimeType,
			Size:     len(f.Data),
			Data:     f.Data,
		}
		joins[i] = LLMCallFile{CallID: callID, SHA256: f.SHA256, Seq: i}
	}
	if err := l.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
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
	if l.retain <= 0 {
		if err := l.db.Model(&LLMCall{}).
			Where("request_json IS NOT NULL OR response_json IS NOT NULL").
			UpdateColumns(map[string]any{
				"request_json":  gorm.Expr("NULL"),
				"response_json": gorm.Expr("NULL"),
			}).Error; err != nil {
			return err
		}
		return l.pruneFiles(pruneFileScope(l.retain, 0))
	}
	var cutoff uint64
	if err := l.db.Model(&LLMCall{}).Select("id").Order("id DESC").Offset(l.retain - 1).Limit(1).Scan(&cutoff).Error; err != nil {
		return err
	}
	if cutoff == 0 {
		return nil
	}
	if err := l.db.Model(&LLMCall{}).
		Where("id < ? AND (request_json IS NOT NULL OR response_json IS NOT NULL)", cutoff).
		UpdateColumns(map[string]any{
			"request_json":  gorm.Expr("NULL"),
			"response_json": gorm.Expr("NULL"),
		}).Error; err != nil {
		return err
	}
	return l.pruneFiles(pruneFileScope(l.retain, cutoff))
}

func (l *gormCallLogger) pruneFiles(before uint64, all bool) error {
	q := l.db
	if all {
		q = q.Where("1 = 1")
	} else {
		q = q.Where("call_id < ?", before)
	}
	if err := q.Delete(&LLMCallFile{}).Error; err != nil {
		return err
	}
	// Only GC files older than fileGCMinAge: a concurrent Record may have
	// inserted an llm_files row whose llm_call_files join is not written yet.
	return l.db.
		Where("created_at < ?", time.Now().UTC().Add(-fileGCMinAge)).
		Where("sha256 NOT IN (?)", l.db.Model(&LLMCallFile{}).Select("sha256")).
		Delete(&LLMFile{}).Error
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
