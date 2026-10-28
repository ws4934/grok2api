package media

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrAssetNotFound         = errors.New("媒体资源不存在")
	ErrInvalidImage          = errors.New("图片内容无效")
	ErrInvalidImageSelection = errors.New("图片选择无效")
	ErrInvalidJobSelection   = errors.New("媒体任务选择无效")
	ErrActiveJobSelection    = errors.New("排队中或生成中的媒体任务不能删除")
	ErrPendingJobUsage       = errors.New("任务正在结算，请稍后再删除")
	ErrJobStateChanged       = errors.New("任务状态已变化，请刷新后重试")
	ErrInvalidFilter         = errors.New("媒体筛选条件无效")
	ErrMediaJobsUnavailable  = errors.New("媒体任务仓储未配置")
	ErrInputAssetNotFound    = errors.New("临时输入资产不存在或已过期")
	ErrMediaCapacity         = errors.New("媒体存储容量不足")
	ErrAssetInUse            = errors.New("媒体资源正被进行中的任务引用")
)

// InputAssetTTL 是临时输入的硬保留上限；无论任务状态如何，超过后都可回收。
const InputAssetTTL = 24 * time.Hour

// Service 负责图片/视频校验、文件落盘和元数据持久化的一致性收口。
type Service struct {
	assets        repository.MediaAssetRepository
	jobs          repository.MediaJobRepository
	tickets       repository.MediaUploadTicketRepository
	objects       repository.MediaObjectStorage
	cleanupLock   repository.DistributedLock
	publicBaseURL string
	configMu      sync.RWMutex
	maxImageBytes int64
	maxTotalBytes int64
	cleanupAt     int
	cleanupEvery  time.Duration
	cleanupSignal chan struct{}
	configChanged chan struct{}
	totalBytes    atomic.Int64
	inputSaveMu   sync.Mutex
}

type Config struct {
	PublicBaseURL           string
	MaxImageBytes           int64
	MaxTotalBytes           int64
	CleanupThresholdPercent int
	CleanupInterval         time.Duration
}

type ImageStats struct {
	TotalImages int64
	TotalBytes  int64
}

type JobStats struct {
	TotalJobs  int64
	Completed  int64
	Failed     int64
	InProgress int64
	Queued     int64
	Canceled   int64
	Uncertain  int64
}

func NewService(assets repository.MediaAssetRepository, jobs repository.MediaJobRepository, objects repository.MediaObjectStorage, cleanupLock repository.DistributedLock, cfg Config) *Service {
	return NewServiceWithTickets(assets, jobs, nil, objects, cleanupLock, cfg)
}

// NewServiceWithTickets 构造包含视频上传票据能力的媒体服务。
func NewServiceWithTickets(assets repository.MediaAssetRepository, jobs repository.MediaJobRepository, tickets repository.MediaUploadTicketRepository, objects repository.MediaObjectStorage, cleanupLock repository.DistributedLock, cfg Config) *Service {
	return &Service{
		assets: assets, jobs: jobs, tickets: tickets, objects: objects, cleanupLock: cleanupLock,
		publicBaseURL: strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/"), maxImageBytes: cfg.MaxImageBytes,
		maxTotalBytes: cfg.MaxTotalBytes, cleanupAt: cfg.CleanupThresholdPercent, cleanupEvery: cfg.CleanupInterval,
		cleanupSignal: make(chan struct{}, 1), configChanged: make(chan struct{}, 1),
	}
}

// UpdateConfig 热更新媒体容量和清理策略，不重建底层存储实例。
func (s *Service) UpdateConfig(cfg Config) {
	s.configMu.Lock()
	s.publicBaseURL = strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	s.maxImageBytes = cfg.MaxImageBytes
	s.maxTotalBytes = cfg.MaxTotalBytes
	s.cleanupAt = cfg.CleanupThresholdPercent
	s.cleanupEvery = cfg.CleanupInterval
	s.configMu.Unlock()
	select {
	case s.configChanged <- struct{}{}:
	default:
	}
}

// SaveImage 校验并保存一份不可变图片，文件写入失败或元数据落库失败时不会留下半成品。
func (s *Service) SaveImage(ctx context.Context, data []byte) (mediadomain.Asset, error) {
	return s.saveImage(ctx, data, nil, "img_")
}

// SaveInputImage 保存不会进入图库、不会公开读取并会自动过期的视频输入图片。
func (s *Service) SaveInputImage(ctx context.Context, data []byte) (mediadomain.Asset, error) {
	cfg := s.runtimeConfig()
	inputLimit := min(cfg.MaxImageBytes, int64(mediadomain.MaxInputAssetBytes))
	if len(data) == 0 || inputLimit <= 0 || int64(len(data)) > inputLimit {
		return mediadomain.Asset{}, ErrInvalidImage
	}
	s.inputSaveMu.Lock()
	defer s.inputSaveMu.Unlock()
	total, err := s.assets.TotalMediaAssetBytes(ctx)
	if err != nil {
		return mediadomain.Asset{}, err
	}
	capacityLimit := cleanupThresholdBytes(cfg)
	if capacityLimit <= 0 || capacityLimit > cfg.MaxTotalBytes {
		capacityLimit = cfg.MaxTotalBytes
	}
	if int64(len(data)) > capacityLimit || total > capacityLimit-int64(len(data)) {
		select {
		case s.cleanupSignal <- struct{}{}:
		default:
		}
		return mediadomain.Asset{}, ErrMediaCapacity
	}
	expiresAt := time.Now().UTC().Add(InputAssetTTL)
	return s.saveImage(ctx, data, &expiresAt, mediadomain.InputAssetIDPrefix)
}

func (s *Service) saveImage(ctx context.Context, data []byte, expiresAt *time.Time, idPrefix string) (mediadomain.Asset, error) {
	cfg := s.runtimeConfig()
	if len(data) == 0 || int64(len(data)) > cfg.MaxImageBytes {
		return mediadomain.Asset{}, ErrInvalidImage
	}
	mimeType := http.DetectContentType(data)
	if !supportedImageMIME(mimeType) {
		return mediadomain.Asset{}, ErrInvalidImage
	}
	id, err := newAssetID(idPrefix)
	if err != nil {
		return mediadomain.Asset{}, err
	}
	digest := sha256.Sum256(data)
	createdAt := time.Now().UTC()
	storageKey, err := s.objects.SaveImage(ctx, id, mimeType, data)
	if err != nil {
		return mediadomain.Asset{}, err
	}
	asset := mediadomain.Asset{
		ID: id, Kind: "image", StorageKey: storageKey, MIMEType: mimeType,
		SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), ExpiresAt: expiresAt, CreatedAt: createdAt,
	}
	asset.Origin, _ = mediadomain.OriginFrom(ctx)
	if err := s.assets.CreateMediaAsset(ctx, asset); err != nil {
		_ = s.objects.Delete(context.WithoutCancel(ctx), storageKey)
		return mediadomain.Asset{}, err
	}
	if s.totalBytes.Add(asset.SizeBytes) > cleanupThresholdBytes(cfg) {
		select {
		case s.cleanupSignal <- struct{}{}:
		default:
		}
	}
	return asset, nil
}

// OpenInputAsset 读取未过期的临时输入；它从不通过公开媒体路由暴露。
func (s *Service) OpenInputAsset(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error) {
	id = strings.TrimSpace(id)
	if !mediadomain.IsInputAssetID(id) {
		return mediadomain.Asset{}, nil, ErrInputAssetNotFound
	}
	asset, err := s.assets.GetMediaAsset(ctx, id)
	if errors.Is(err, repository.ErrNotFound) {
		return mediadomain.Asset{}, nil, ErrInputAssetNotFound
	}
	if err != nil {
		return mediadomain.Asset{}, nil, err
	}
	if (asset.Kind != "image" && asset.Kind != "video") || asset.ExpiresAt == nil {
		return mediadomain.Asset{}, nil, ErrInputAssetNotFound
	}
	if !asset.ExpiresAt.After(time.Now().UTC()) {
		return mediadomain.Asset{}, nil, ErrInputAssetNotFound
	}
	body, err := s.objects.Open(ctx, asset.StorageKey)
	if errors.Is(err, os.ErrNotExist) {
		return mediadomain.Asset{}, nil, ErrInputAssetNotFound
	}
	if err != nil {
		return mediadomain.Asset{}, nil, err
	}
	return asset, body, nil
}

// ReleaseInputAssets 在任务进入终态后立即回收不再被其他活动任务引用的临时输入。
func (s *Service) ReleaseInputAssets(ctx context.Context, references []string) error {
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		id, ok := mediadomain.ParseInputReference(reference)
		if !ok {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		expired, expireErr := s.assets.ExpireMediaInputIfUnreferenced(ctx, id, time.Now().UTC())
		if expireErr != nil {
			return expireErr
		}
		if !expired {
			continue
		}
		asset, getErr := s.assets.GetMediaAsset(ctx, id)
		if errors.Is(getErr, repository.ErrNotFound) {
			continue
		}
		if getErr != nil {
			return getErr
		}
		if asset.ExpiresAt == nil || (asset.Kind != "image" && asset.Kind != "video") {
			continue
		}
		if deleteErr := s.objects.Delete(ctx, asset.StorageKey); deleteErr != nil && !errors.Is(deleteErr, os.ErrNotExist) {
			return deleteErr
		}
		if deleteErr := s.assets.DeleteMediaAsset(ctx, id); deleteErr != nil && !errors.Is(deleteErr, repository.ErrNotFound) {
			return deleteErr
		}
	}
	if total, totalErr := s.assets.TotalMediaAssetBytes(ctx); totalErr == nil {
		s.totalBytes.Store(total)
	}
	return nil
}

// PublicImageURL 返回可直接用于图片展示的公开资源地址。
func (s *Service) PublicImageURL(id string) string {
	return s.runtimeConfig().PublicBaseURL + "/v1/media/images/" + id
}

// PublicVideoURL 返回可直接用于视频播放的公开资源地址。
func (s *Service) PublicVideoURL(id string) string {
	return s.runtimeConfig().PublicBaseURL + "/v1/media/videos/" + id
}

// OpenImage 读取图片元数据和正文，不向调用方暴露实际文件路径。
func (s *Service) OpenImage(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error) {
	asset, err := s.assets.GetMediaAsset(ctx, strings.TrimSpace(id))
	if errors.Is(err, repository.ErrNotFound) {
		return mediadomain.Asset{}, nil, ErrAssetNotFound
	}
	if err != nil {
		return mediadomain.Asset{}, nil, err
	}
	if asset.Kind != "image" || asset.ExpiresAt != nil {
		return mediadomain.Asset{}, nil, ErrAssetNotFound
	}
	body, err := s.objects.Open(ctx, asset.StorageKey)
	if errors.Is(err, os.ErrNotExist) {
		return mediadomain.Asset{}, nil, ErrAssetNotFound
	}
	if err != nil {
		return mediadomain.Asset{}, nil, err
	}
	return asset, body, nil
}

// AdminListImages 分页返回图库资源列表；filter.Kind 为空时只列图片。
func (s *Service) AdminListImages(ctx context.Context, page, pageSize int, search string, filter repository.MediaAssetListFilter) ([]mediadomain.Asset, int64, error) {
	if filter.Kind != "" && filter.Kind != "image" && filter.Kind != "video" {
		return nil, 0, ErrInvalidFilter
	}
	return s.assets.ListMediaAssets(ctx, repository.MediaAssetListQuery{Page: mediaPageQuery(page, pageSize, search, repository.SortQuery{}), Filter: filter})
}

// AdminGetAsset 返回一份图库资产（含来源）；临时输入不属于图库。
func (s *Service) AdminGetAsset(ctx context.Context, id string) (mediadomain.Asset, error) {
	asset, err := s.assets.GetMediaAsset(ctx, strings.TrimSpace(id))
	if errors.Is(err, repository.ErrNotFound) || (err == nil && asset.ExpiresAt != nil) {
		return mediadomain.Asset{}, ErrAssetNotFound
	}
	return asset, err
}

// AdminListJobs 分页返回图片与视频任务列表。
func (s *Service) AdminListJobs(ctx context.Context, page, pageSize int, search string, filter repository.MediaJobListFilter, sort repository.SortQuery) ([]mediadomain.Job, int64, error) {
	if s.jobs == nil {
		return nil, 0, ErrMediaJobsUnavailable
	}
	filter.Kind, filter.Status, filter.Provider = strings.TrimSpace(filter.Kind), strings.TrimSpace(filter.Status), strings.TrimSpace(filter.Provider)
	if !validJobKind(filter.Kind) || !validMediaStatus(filter.Status) || (filter.Provider != "" && !accountdomain.Provider(filter.Provider).IsValid()) ||
		!repository.IsValidSort(sort, "prompt", "model", "status", "progress", "spec", "account", "createdAt", "completedAt") {
		return nil, 0, ErrInvalidFilter
	}
	return s.jobs.ListMediaJobs(ctx, repository.MediaJobListQuery{Page: mediaPageQuery(page, pageSize, search, sort), Filter: filter})
}

// AdminImageStats 返回图片统计信息。
func (s *Service) AdminImageStats(ctx context.Context) (ImageStats, error) {
	stats, err := s.assets.SummarizeMediaAssets(ctx)
	if err != nil {
		return ImageStats{}, err
	}
	return ImageStats{TotalImages: stats.TotalImages, TotalBytes: stats.TotalBytes}, nil
}

// AdminDeleteImages 删除管理员明确选择的图库资产（图片或视频）对象及其元数据。
// 不存在、已被并发清理或不属于图库的临时输入按幂等成功处理。被活动任务结果或
// 未消费上传票据引用的资产与容量清理同样受保护：整批拒绝并返回 ErrAssetInUse，
// 单个与批量删除行为一致。终态任务对已删资产的引用由读取端容忍（同容量清理）。
func (s *Service) AdminDeleteImages(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return 0, ErrInvalidImageSelection
	}
	unique := make(map[string]struct{}, len(ids))
	assets := make([]mediadomain.Asset, 0, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return 0, ErrInvalidImageSelection
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		asset, err := s.assets.GetMediaAsset(ctx, id)
		if errors.Is(err, repository.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if asset.ExpiresAt == nil {
			assets = append(assets, asset)
		}
	}
	if len(assets) > 0 {
		protected, err := s.assets.ListProtectedMediaAssetIDs(ctx)
		if err != nil {
			return 0, err
		}
		for _, asset := range assets {
			if _, inUse := protected[asset.ID]; inUse {
				return 0, ErrAssetInUse
			}
		}
	}

	deleted := 0
	for _, asset := range assets {
		if err := s.objects.Delete(ctx, asset.StorageKey); err != nil && !errors.Is(err, os.ErrNotExist) {
			return deleted, err
		}
		if err := s.assets.DeleteMediaAsset(ctx, asset.ID); err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			return deleted, err
		}
		deleted++
	}
	if total, err := s.assets.TotalMediaAssetBytes(ctx); err == nil {
		s.totalBytes.Store(total)
	}
	return deleted, nil
}

// AdminJobStats 返回媒体任务各状态统计；kind 为空时统计全部类型。
func (s *Service) AdminJobStats(ctx context.Context, kind string) (JobStats, error) {
	if s.jobs == nil {
		return JobStats{}, ErrMediaJobsUnavailable
	}
	if kind = strings.TrimSpace(kind); !validJobKind(kind) {
		return JobStats{}, ErrInvalidFilter
	}
	stats, err := s.jobs.SummarizeMediaJobs(ctx, kind)
	if err != nil {
		return JobStats{}, err
	}
	return JobStats(stats), nil
}

// AdminDeleteJobs 删除管理员明确选择的终态媒体任务。
// 视频任务若已归档本地视频，则同步撤销上传票据并删除对象和媒体元数据；
// 图片任务的结果属于图库，保留不删。审计记录独立保留。
func (s *Service) AdminDeleteJobs(ctx context.Context, ids []string) (int, error) {
	if s.jobs == nil {
		return 0, ErrMediaJobsUnavailable
	}
	if len(ids) == 0 || len(ids) > 100 {
		return 0, ErrInvalidJobSelection
	}
	unique := make(map[string]struct{}, len(ids))
	normalized := make([]string, 0, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return 0, ErrInvalidJobSelection
		}
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		normalized = append(normalized, id)
	}
	jobs, err := s.jobs.GetMediaJobsByIDs(ctx, normalized)
	if err != nil {
		return 0, err
	}
	for _, job := range jobs {
		if !job.Status.Terminal() {
			return 0, ErrActiveJobSelection
		}
		if job.UsagePending() {
			return 0, ErrPendingJobUsage
		}
	}

	deleted := 0
	for _, job := range jobs {
		if s.tickets != nil {
			if err := s.tickets.DeleteUploadTicketsByJobID(ctx, job.ID); err != nil {
				return deleted, err
			}
		}
		if err := s.deleteVideoAsset(ctx, job.ResultAssetID); err != nil {
			return deleted, err
		}
		if err := s.jobs.DeleteMediaJob(ctx, job.ID); err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				continue
			}
			if errors.Is(err, repository.ErrConflict) {
				return deleted, ErrJobStateChanged
			}
			return deleted, err
		}
		deleted++
	}
	if total, err := s.assets.TotalMediaAssetBytes(ctx); err == nil {
		s.totalBytes.Store(total)
	}
	return deleted, nil
}

// deleteVideoAsset 删除任务绑定的本地视频对象与元数据；缺失对象按幂等成功处理。
func (s *Service) deleteVideoAsset(ctx context.Context, assetID string) error {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return nil
	}
	asset, err := s.assets.GetMediaAsset(ctx, assetID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if asset.Kind != "video" {
		return nil
	}
	if err := s.objects.Delete(ctx, asset.StorageKey); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := s.assets.DeleteMediaAsset(ctx, asset.ID); err != nil && !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	return nil
}

func mediaPageQuery(page, pageSize int, search string, sort repository.SortQuery) repository.PageQuery {
	page, pageSize = repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
	return repository.PageQuery{Offset: (page - 1) * pageSize, Limit: pageSize, Search: strings.TrimSpace(search), Sort: sort}
}

func validMediaStatus(status string) bool {
	switch mediadomain.Status(status) {
	case "", mediadomain.StatusQueued, mediadomain.StatusInProgress, mediadomain.StatusCompleted, mediadomain.StatusFailed, mediadomain.StatusCanceled, mediadomain.StatusUncertain:
		return true
	default:
		return false
	}
}

func validJobKind(kind string) bool {
	return kind == "" || kind == string(mediadomain.KindVideo) || kind == string(mediadomain.KindImage)
}

// RunCleanup 响应容量阈值并按周期清理最旧媒体资源。
func (s *Service) RunCleanup(ctx context.Context, onError func(error)) {
	cfg := s.runtimeConfig()
	if total, err := s.assets.TotalMediaAssetBytes(ctx); err == nil {
		s.totalBytes.Store(total)
		if total > cleanupThresholdBytes(cfg) {
			if _, cleanupErr := s.Cleanup(ctx); cleanupErr != nil && onError != nil {
				onError(cleanupErr)
			}
		}
	} else if onError != nil {
		onError(err)
	}
	ticker := time.NewTicker(cfg.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.cleanupSignal:
		case <-s.configChanged:
			cfg = s.runtimeConfig()
			ticker.Reset(cfg.CleanupInterval)
		}
		cfg = s.runtimeConfig()
		cleanupCtx, cancel := context.WithTimeout(ctx, min(cfg.CleanupInterval, 5*time.Minute))
		_, err := s.Cleanup(cleanupCtx)
		cancel()
		if err != nil && onError != nil {
			onError(err)
		}
	}
}

const (
	cleanupAssetBatchSize  = 200
	cleanupTicketBatchSize = 200
	cleanupInputBatchSize  = 200
	// cleanupTicketMaxBatchesPerRun 限制单次 Cleanup 调用中过期票据的删除批次数。
	// 超出部分留给后续直接/后台清理继续回收，避免一次调用无界垄断。
	cleanupTicketMaxBatchesPerRun = 1
)

// Cleanup 在跨实例锁保护下删除最旧媒体资产，直到回落到自动清理阈值。
// 同时有界清理过期上传票据。受保护资产被跳过；通过 offset 前进避免整页受保护时死循环。
func (s *Service) Cleanup(ctx context.Context) (int, error) {
	cfg := s.runtimeConfig()
	if s.cleanupLock != nil {
		release, acquired, err := s.cleanupLock.Acquire(ctx, "media:cleanup", 30*time.Minute)
		if err != nil || !acquired {
			return 0, err
		}
		defer release()
	}
	deleted := 0
	cleanupNow := time.Now().UTC()
	// 临时输入无论当前总容量或任务状态如何都按 24h 硬 TTL 回收。
	expiredOffset := 0
	for {
		expired, listErr := s.assets.ListExpiredMediaAssets(ctx, cleanupNow, expiredOffset, cleanupInputBatchSize)
		if listErr != nil {
			return deleted, listErr
		}
		if len(expired) == 0 {
			break
		}
		deletedInBatch := 0
		for _, asset := range expired {
			if err := s.objects.Delete(ctx, asset.StorageKey); err != nil && !errors.Is(err, os.ErrNotExist) {
				return deleted, err
			}
			if err := s.assets.DeleteMediaAsset(ctx, asset.ID); err != nil && !errors.Is(err, repository.ErrNotFound) {
				return deleted, err
			}
			deleted++
			deletedInBatch++
		}
		if deletedInBatch > 0 {
			// 删除后分页顺序前移，从头重新扫描避免跳过记录。
			expiredOffset = 0
			continue
		}
		if len(expired) < cleanupInputBatchSize {
			break
		}
		expiredOffset += len(expired)
	}
	// 过期票据回收：不触碰未过期票据与已登记媒体资产；单次调用有批次数上限。
	if s.tickets != nil {
		for batch := 0; batch < cleanupTicketMaxBatchesPerRun; batch++ {
			n, err := s.tickets.DeleteExpiredUploadTickets(ctx, time.Now().UTC(), cleanupTicketBatchSize)
			if err != nil {
				return deleted, err
			}
			if n < int64(cleanupTicketBatchSize) {
				break
			}
		}
	}
	total, err := s.assets.TotalMediaAssetBytes(ctx)
	if err != nil {
		return deleted, err
	}
	s.totalBytes.Store(total)
	threshold := cleanupThresholdBytes(cfg)
	if total <= threshold {
		return deleted, nil
	}
	offset := 0
	for total > threshold {
		values, err := s.assets.ListOldestMediaAssets(ctx, offset, cleanupAssetBatchSize)
		if err != nil {
			return deleted, err
		}
		if len(values) == 0 {
			break
		}
		protected, protErr := s.assets.ListProtectedMediaAssetIDs(ctx)
		if protErr != nil {
			return deleted, protErr
		}
		deletedInBatch := 0
		for _, asset := range values {
			if total <= threshold {
				break
			}
			// 未过期的临时输入有明确 TTL，容量清理不得提前删除。
			if asset.ExpiresAt != nil && asset.ExpiresAt.After(cleanupNow) {
				continue
			}
			if _, skip := protected[asset.ID]; skip {
				continue
			}
			if err := s.objects.Delete(ctx, asset.StorageKey); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return deleted, fmt.Errorf("媒体对象缺失，已保留共享元数据: %s: %w", asset.StorageKey, err)
				}
				return deleted, err
			}
			if err := s.assets.DeleteMediaAsset(ctx, asset.ID); err != nil && !errors.Is(err, repository.ErrNotFound) {
				return deleted, err
			}
			total = max(0, total-asset.SizeBytes)
			deleted++
			deletedInBatch++
		}
		if deletedInBatch == 0 {
			// 本页无可删资产：前进 offset 以越过受保护前缀，避免无限循环。
			if len(values) < cleanupAssetBatchSize {
				break
			}
			offset += len(values)
			continue
		}
		// 删除后最旧顺序变化，从头部重新扫描。
		offset = 0
	}
	s.totalBytes.Store(total)
	return deleted, nil
}

func (s *Service) runtimeConfig() Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return Config{
		PublicBaseURL: s.publicBaseURL,
		MaxImageBytes: s.maxImageBytes, MaxTotalBytes: s.maxTotalBytes,
		CleanupThresholdPercent: s.cleanupAt, CleanupInterval: s.cleanupEvery,
	}
}

func cleanupThresholdBytes(cfg Config) int64 {
	return cfg.MaxTotalBytes * int64(cfg.CleanupThresholdPercent) / 100
}

func newAssetID(prefix string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成媒体资源 ID: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func supportedImageMIME(value string) bool {
	switch value {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}
