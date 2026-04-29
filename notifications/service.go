package notifications

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
)

// Default tunables — overridable via Config.
const (
	defaultWorkerCount     = 3
	defaultPollInterval    = 2 * time.Second
	defaultRetryBaseDelay  = 30 * time.Second
	defaultMaxRetries      = 3
	defaultDedupWindow     = 60 * time.Second
	defaultQueueBufferSize = 200
)

// Config wires the Service.  All fields except DB are optional.
type Config struct {
	DB *gorm.DB

	// WorkerCount is the number of concurrent delivery workers.
	WorkerCount int
	// PollInterval is how often the recovery poller scans for pending entries.
	PollInterval time.Duration
	// RetryBaseDelay multiplied by the attempt count gives the next-retry delay.
	RetryBaseDelay time.Duration
	// MaxRetries is the default cap when EnqueueRequest doesn't set one.
	MaxRetries int
	// DedupWindow is how far back the dedup check looks.
	DedupWindow time.Duration
	// QueueBufferSize sizes the in-memory worker signal channel.
	QueueBufferSize int

	// Logger is used for warnings & operational messages.  If nil, the std
	// logger is used.
	Logger *log.Logger
}

// Service is the persistent notification queue.  Construct with New, register
// channel handlers, then call Start (or rely on Enqueue's lazy starter).
type Service struct {
	cfg    Config
	db     *gorm.DB
	logger *log.Logger

	queue    chan uuid.UUID
	stop     chan struct{}
	wg       sync.WaitGroup
	channels *channelRegistry

	// dedupGroup coalesces concurrent Enqueue calls that share the same key
	// so the check-then-create can't be raced into duplicate rows.
	dedupGroup singleflight.Group

	startOnce sync.Once
	stopOnce  sync.Once
}

// New creates a Service.  Call Service.Start() to launch background workers,
// or use Enqueue which lazily starts on first call.
func New(cfg Config) *Service {
	if cfg.DB == nil {
		panic("notifications: Config.DB is required")
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = defaultWorkerCount
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.RetryBaseDelay <= 0 {
		cfg.RetryBaseDelay = defaultRetryBaseDelay
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	if cfg.DedupWindow <= 0 {
		cfg.DedupWindow = defaultDedupWindow
	}
	if cfg.QueueBufferSize <= 0 {
		cfg.QueueBufferSize = defaultQueueBufferSize
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Service{
		cfg:      cfg,
		db:       cfg.DB,
		logger:   logger,
		queue:    make(chan uuid.UUID, cfg.QueueBufferSize),
		stop:     make(chan struct{}),
		channels: newChannelRegistry(),
	}
}

// Register binds a ChannelHandler to a channel name.  Subsequent enqueues
// with the same channel will dispatch to this handler.  Safe to call before
// or after Start.
func (s *Service) Register(channel string, h ChannelHandler) {
	if channel == "" || h == nil {
		return
	}
	s.channels.set(channel, h)
}

// Start launches workers and the recovery poller.  Idempotent (safe to call
// multiple times and concurrently — sync.Once gates the actual launch).
func (s *Service) Start() {
	s.startOnce.Do(func() {
		for i := 0; i < s.cfg.WorkerCount; i++ {
			s.wg.Add(1)
			go s.worker()
		}
		s.wg.Add(1)
		go s.recoveryPoller()
		s.logger.Printf("notifications: started (%d workers, poll=%s)", s.cfg.WorkerCount, s.cfg.PollInterval)
	})
}

// Shutdown stops workers and waits for in-flight deliveries to complete.
func (s *Service) Shutdown() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.wg.Wait()
		s.logger.Printf("notifications: shut down")
	})
}

// EnqueueRequest is the input shape for Enqueue.  Required: OrganizationID,
// Channel, Message.  Event/Target/Source are recommended but optional.
type EnqueueRequest struct {
	OrganizationID uuid.UUID
	SourceType     string
	SourceID       *uuid.UUID
	SourceName     string
	Event          string
	Channel        string
	Target         string
	Message        string
	ContextRef     string
	HandlerHint    string

	// MaxRetries overrides the Service default; <= 0 uses the default.
	MaxRetries int

	// DedupKey overrides the auto-computed key.  Provide your own when the
	// natural identity isn't event|channel|target|message (e.g. you want to
	// dedup by user_id+content_hash instead).
	DedupKey string
}

// EnqueueResult reports the outcome of an Enqueue call.
type EnqueueResult struct {
	// EntryID of the created row, or uuid.Nil when Deduplicated == true.
	EntryID uuid.UUID
	// Deduplicated reports whether an existing entry within the dedup window
	// matched and the new request was therefore dropped.
	Deduplicated bool
}

// Enqueue persists a notification and signals a worker.  Lazily Starts the
// service if not yet running.  Concurrent calls with the same DedupKey are
// coalesced via singleflight so the check-then-create cannot race.
func (s *Service) Enqueue(ctx context.Context, req EnqueueRequest) (EnqueueResult, error) {
	if req.Channel == "" {
		return EnqueueResult{}, errors.New("notifications: channel is required")
	}
	if req.Message == "" {
		return EnqueueResult{}, errors.New("notifications: message is required")
	}

	// Lazy start so naive callers don't have to remember Start(). Start() is
	// gated by sync.Once so unconditional invocation is cheap and race-free.
	s.Start()

	dedupKey := req.DedupKey
	if dedupKey == "" {
		dedupKey = BuildDedupKey(req.Event, req.Channel, req.Target, req.Message)
	}

	maxRetries := req.MaxRetries
	if maxRetries <= 0 {
		maxRetries = s.cfg.MaxRetries
	}

	// singleflight coalesces concurrent enqueues sharing the same dedup key.
	sfKey := fmt.Sprintf("%s|%s", req.OrganizationID, dedupKey)
	v, err, _ := s.dedupGroup.Do(sfKey, func() (any, error) {
		return s.enqueueLocked(ctx, req, dedupKey, maxRetries)
	})
	if err != nil {
		return EnqueueResult{}, err
	}
	return v.(EnqueueResult), nil
}

func (s *Service) enqueueLocked(ctx context.Context, req EnqueueRequest, dedupKey string, maxRetries int) (EnqueueResult, error) {
	// Dedup check.
	var existing QueueEntry
	cutoff := time.Now().Add(-s.cfg.DedupWindow)
	err := s.db.WithContext(ctx).
		Where("dedup_key = ? AND organization_id = ? AND created_at > ? AND status IN ?",
			dedupKey, req.OrganizationID, cutoff,
			[]string{StatusPending, StatusProcessing, StatusSent}).
		First(&existing).Error
	if err == nil {
		s.logger.Printf("notifications: dedup hit (org=%s event=%s channel=%s key=%s)",
			req.OrganizationID, req.Event, req.Channel, dedupKey)
		return EnqueueResult{Deduplicated: true}, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return EnqueueResult{}, fmt.Errorf("notifications: dedup query: %w", err)
	}

	entry := QueueEntry{
		SourceType:  req.SourceType,
		SourceID:    req.SourceID,
		SourceName:  req.SourceName,
		Event:       req.Event,
		Channel:     req.Channel,
		Target:      req.Target,
		Message:     req.Message,
		ContextRef:  req.ContextRef,
		HandlerHint: req.HandlerHint,
		Status:      StatusPending,
		MaxRetries:  maxRetries,
		DedupKey:    dedupKey,
	}
	entry.OrganizationID = req.OrganizationID
	if err := s.db.WithContext(ctx).Create(&entry).Error; err != nil {
		return EnqueueResult{}, fmt.Errorf("notifications: insert: %w", err)
	}

	select {
	case s.queue <- entry.ID:
	default:
		// Buffer full — recovery poller will pick it up.
	}
	return EnqueueResult{EntryID: entry.ID}, nil
}

// worker pulls IDs from the in-memory channel and delivers them.
func (s *Service) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stop:
			return
		case entryID := <-s.queue:
			s.processEntry(entryID)
		}
	}
}

func (s *Service) processEntry(entryID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var entry QueueEntry
	if err := s.db.WithContext(ctx).First(&entry, "id = ?", entryID).Error; err != nil {
		return
	}
	if entry.Status == StatusSent {
		return
	}

	// Atomic claim — guards against multiple workers grabbing the same row.
	// Only Pending is reclaimable: transient failures get bounced back to
	// Pending in markFailed; StatusFailed is terminal (permanent error or
	// max-retries hit) and must never be replayed, otherwise a permanent
	// failure can be retried on a races where the recovery poller and the
	// in-memory queue both signal the same entry.
	res := s.db.WithContext(ctx).Model(&entry).
		Where("status = ?", StatusPending).
		Update("status", StatusProcessing)
	if res.RowsAffected == 0 {
		return
	}

	entry.Attempts++

	handler, ok := s.channels.get(entry.Channel)
	if !ok {
		s.markFailed(ctx, &entry, fmt.Errorf("no handler registered for channel %q", entry.Channel), true)
		return
	}

	deliveryErr := handler.Deliver(ctx, &entry)
	if deliveryErr != nil {
		s.markFailed(ctx, &entry, deliveryErr, IsPermanent(deliveryErr))
		return
	}
	s.markSent(ctx, &entry)
}

func (s *Service) markSent(ctx context.Context, entry *QueueEntry) {
	now := time.Now()
	s.db.WithContext(ctx).Model(entry).Updates(map[string]any{
		"status":   StatusSent,
		"attempts": entry.Attempts,
		"sent_at":  now,
		"error":    "",
	})
	s.logger.Printf("notifications: sent (channel=%s target=%s event=%s)",
		entry.Channel, entry.Target, entry.Event)
}

// markFailed records the failure and either schedules a retry or marks the
// entry permanently failed.  permanent=true skips the retry bookkeeping.
func (s *Service) markFailed(ctx context.Context, entry *QueueEntry, err error, permanent bool) {
	errMsg := err.Error()
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	updates := map[string]any{
		"attempts": entry.Attempts,
		"error":    errMsg,
	}
	if !permanent && entry.Attempts < entry.MaxRetries {
		nextRetry := time.Now().Add(s.cfg.RetryBaseDelay * time.Duration(entry.Attempts))
		updates["status"] = StatusPending
		updates["next_retry"] = nextRetry
		s.logger.Printf("notifications: failed (attempt %d/%d, retry at %s): %v",
			entry.Attempts, entry.MaxRetries, nextRetry.Format(time.RFC3339), err)
	} else {
		updates["status"] = StatusFailed
		s.logger.Printf("notifications: permanently failed after %d attempts: %v",
			entry.Attempts, err)
	}
	s.db.WithContext(ctx).Model(entry).Updates(updates)
}

// recoveryPoller periodically signals workers to drain pending entries.
// Handles startup recovery, retry-ready entries, and overflow from the
// in-memory signal channel.
func (s *Service) recoveryPoller() {
	defer s.wg.Done()
	s.recoverPending()

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.recoverPending()
		}
	}
}

func (s *Service) recoverPending() {
	var entries []QueueEntry
	now := time.Now()
	s.db.Where("status = ? AND (next_retry IS NULL OR next_retry <= ?)", StatusPending, now).
		Order("created_at ASC").
		Limit(50).
		Find(&entries)
	for _, e := range entries {
		select {
		case s.queue <- e.ID:
		default:
			return
		}
	}
}

// BuildDedupKey hashes the canonical dedup tuple to a stable 32-char key.
// Exposed so callers can pre-compute the key (useful for "did I already
// enqueue this?" checks before paying the cost of building the message).
func BuildDedupKey(event, channel, target, message string) string {
	raw := fmt.Sprintf("%s|%s|%s|%s", event, channel, target, message)
	hash := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", hash[:16])
}
