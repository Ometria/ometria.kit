// Package firehose sends publicapi's request log to an Amazon Data Firehose
// stream, as PHP's ometria.api does when API_LOG_FIREHOSE_STREAM is set.
//
// Each publicapi.Record becomes one Firehose record whose data is the
// record's JSON. It has the keys, order, types and values of PHP's
// json_encode of its row, but not its byte-level escaping: PHP writes "/" as
// "\/" and non-ASCII characters as \uXXXX escapes, where Go writes both as
// is and escapes "<", ">" and "&" instead. Consumers that parse the JSON see
// the same values.
//
// Records are queued and sent in the background with PutRecordBatch, in
// batches of up to MaxBatchRecords records or MaxBatchBytes bytes, flushed
// when full or every Config.FlushInterval. The request never waits for
// Firehose and a failure to log never fails it: records that can't be queued
// are dropped, records Firehose rejects are retried once, and Stats counts
// both. Problems are logged as a summary at most once every
// Config.LogInterval.
//
//	client := firehose.NewClient(awsConfig)
//	sink := firehose.New(client, firehose.Config{Stream: os.Getenv("API_LOG_FIREHOSE_STREAM")})
//	defer sink.Close(context.Background())
//
//	handler := publicapi.RequestLog(sink)(routes)
//
// It is a module of its own so that only services that use it depend on the
// AWS SDK.
package firehose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	"github.com/aws/aws-sdk-go-v2/service/firehose/types"

	"github.com/Ometria/ometria.kit/v2/publicapi"
)

// DefaultRegion is the region PHP sends request logs to, used when the AWS
// config has none.
const DefaultRegion = "eu-west-1"

// ConnectTimeout is how long NewClient's client waits to connect, as PHP's
// Firehose client does.
const ConnectTimeout = time.Second

// PutRecordBatch's limits.
const (
	// MaxBatchRecords is the most records one PutRecordBatch call takes.
	MaxBatchRecords = 500
	// MaxBatchBytes is the most data one PutRecordBatch call takes.
	MaxBatchBytes = 4 << 20
	// MaxRecordBytes is the largest record Firehose accepts. Larger records
	// fail without being sent.
	MaxRecordBytes = 1000 << 10
)

// retryDelay is how long a Sink waits before retrying the records Firehose
// rejected.
var retryDelay = 100 * time.Millisecond

// PutRecordBatchAPI is the part of *firehose.Client a Sink uses.
type PutRecordBatchAPI interface {
	PutRecordBatch(ctx context.Context, in *firehose.PutRecordBatchInput, optFns ...func(*firehose.Options)) (*firehose.PutRecordBatchOutput, error)
}

// NewClient returns a Firehose client for cfg with a one second connect
// timeout, in DefaultRegion if cfg has no region.
func NewClient(cfg aws.Config) *firehose.Client {
	return firehose.NewFromConfig(cfg, func(o *firehose.Options) {
		if o.Region == "" {
			o.Region = DefaultRegion
		}
		o.HTTPClient = awshttp.NewBuildableClient().WithDialerOptions(func(d *net.Dialer) {
			d.Timeout = ConnectTimeout
		})
	})
}

// Config configures a Sink.
type Config struct {
	// Stream is the delivery stream's name. Required.
	Stream string
	// Timeout bounds each PutRecordBatch call, the SDK's retries included.
	// Defaults to 2s.
	Timeout time.Duration
	// FlushInterval is the longest a record waits for its batch to fill
	// before the batch is sent anyway. Defaults to 500ms.
	FlushInterval time.Duration
	// QueueSize is how many records can wait to be batched before new ones
	// are dropped. Defaults to 1000.
	QueueSize int
	// Workers is how many batches are sent at once. Defaults to 2.
	Workers int
	// LogInterval is the shortest time between two warnings about dropped or
	// failed records. Each warning sums up those since the last. Defaults to
	// 10s.
	LogInterval time.Duration
	// Logger receives the warnings. Defaults to slog.Default().
	Logger *slog.Logger
}

// Stats counts a Sink's records since it was created.
type Stats struct {
	// Sent is how many records Firehose accepted.
	Sent uint64
	// Dropped is how many records Send couldn't queue, because the queue was
	// full or the Sink closed.
	Dropped uint64
	// Failed is how many queued records were never accepted: Firehose
	// rejected them twice, the call failed, they were too large, or Close
	// gave up on them.
	Failed uint64
}

// Sink is a publicapi.Sink that sends records to Firehose.
type Sink struct {
	client  PutRecordBatchAPI
	cfg     Config
	queue   chan publicapi.Record
	batches chan []types.Record

	// ctx is the base of every call; Close cancels it when it gives up.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{} // Closed once every goroutine has exited.

	mu     sync.RWMutex
	closed bool

	sent, dropped, failed atomic.Uint64

	logMu       sync.Mutex
	lastLog     time.Time
	pendDropped uint64
	pendFailed  uint64
	lastErr     string
}

var _ publicapi.Sink = (*Sink)(nil)

// New returns a Sink sending to client and starts its goroutines. Call
// Close to send the queued records and stop them.
func New(client PutRecordBatchAPI, cfg Config) *Sink {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 500 * time.Millisecond
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1000
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.LogInterval <= 0 {
		cfg.LogInterval = 10 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Sink{
		client:  client,
		cfg:     cfg,
		queue:   make(chan publicapi.Record, cfg.QueueSize),
		batches: make(chan []types.Record),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}

	var wg sync.WaitGroup
	wg.Go(s.batch)
	for range cfg.Workers {
		wg.Go(s.work)
	}
	go func() {
		wg.Wait()
		s.logSummary(true)
		close(s.done)
	}()
	return s
}

// Send queues rec without blocking. It drops rec if the queue is full or
// the Sink is closed.
func (s *Sink) Send(_ context.Context, rec publicapi.Record) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.drop(errors.New("sink closed"))
		return
	}
	select {
	case s.queue <- rec:
	default:
		s.drop(errors.New("queue full"))
	}
}

// Stats returns the Sink's counts so far.
func (s *Sink) Stats() Stats {
	return Stats{Sent: s.sent.Load(), Dropped: s.dropped.Load(), Failed: s.failed.Load()}
}

// Close stops accepting records and waits until the queued ones are sent or
// ctx is done. If ctx is done first, it cancels the calls in flight, counts
// the unsent records as failed and returns ctx's error; the goroutines exit
// soon after.
func (s *Sink) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()

	select {
	case <-s.done:
		s.cancel()
		return nil
	case <-ctx.Done():
		s.cancel()
		return ctx.Err()
	}
}

// batch collects queued records into batches for the workers.
func (s *Sink) batch() {
	defer close(s.batches)
	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()

	var recs []types.Record
	size := 0
	flush := func() {
		if len(recs) == 0 {
			return
		}
		select {
		case s.batches <- recs:
		case <-s.ctx.Done():
			s.fail(len(recs), s.ctx.Err())
		}
		recs, size = nil, 0
	}

	for {
		select {
		case rec, ok := <-s.queue:
			if !ok {
				flush()
				return
			}
			data, err := json.Marshal(rec)
			if err != nil {
				s.fail(1, err)
				continue
			}
			if len(data) > MaxRecordBytes {
				s.fail(1, fmt.Errorf("record of %d bytes is over Firehose's limit", len(data)))
				continue
			}
			if size+len(data) > MaxBatchBytes {
				flush()
			}
			recs = append(recs, types.Record{Data: data})
			size += len(data)
			if len(recs) == MaxBatchRecords {
				flush()
			}
		case <-ticker.C:
			flush()
			s.logSummary(false)
		}
	}
}

func (s *Sink) work() {
	for recs := range s.batches {
		s.put(recs)
	}
}

// put sends recs, and once more those Firehose rejects.
func (s *Sink) put(recs []types.Record) {
	for attempt := 0; ; attempt++ {
		if err := s.ctx.Err(); err != nil {
			s.fail(len(recs), err)
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Timeout)
		out, err := s.client.PutRecordBatch(ctx, &firehose.PutRecordBatchInput{
			DeliveryStreamName: aws.String(s.cfg.Stream),
			Records:            recs,
		})
		cancel()
		if err != nil {
			s.fail(len(recs), err)
			return
		}
		if aws.ToInt32(out.FailedPutCount) == 0 {
			s.sent.Add(uint64(len(recs)))
			return
		}

		retry, err := rejected(recs, out)
		s.sent.Add(uint64(len(recs) - len(retry)))
		if attempt > 0 {
			s.fail(len(retry), err)
			return
		}
		recs = retry
		select {
		case <-time.After(retryDelay):
		case <-s.ctx.Done():
		}
	}
}

// rejected returns the records out marks as failed, and an error describing
// the last of them. If out doesn't say which, they all are.
func rejected(recs []types.Record, out *firehose.PutRecordBatchOutput) ([]types.Record, error) {
	if len(out.RequestResponses) != len(recs) {
		return recs, fmt.Errorf("%d of %d records failed", aws.ToInt32(out.FailedPutCount), len(recs))
	}
	var failed []types.Record
	var err error
	for i, r := range out.RequestResponses {
		if r.ErrorCode != nil {
			failed = append(failed, recs[i])
			err = fmt.Errorf("%s: %s", aws.ToString(r.ErrorCode), aws.ToString(r.ErrorMessage))
		}
	}
	return failed, err
}

func (s *Sink) drop(err error) {
	s.dropped.Add(1)
	s.note(1, 0, err)
}

func (s *Sink) fail(n int, err error) {
	if n == 0 {
		return
	}
	s.failed.Add(uint64(n))
	s.note(0, uint64(n), err)
}

// note adds to the counts of the next summary, and logs it if it's due.
func (s *Sink) note(dropped, failed uint64, err error) {
	s.logMu.Lock()
	s.pendDropped += dropped
	s.pendFailed += failed
	if err != nil {
		s.lastErr = err.Error()
	}
	s.logMu.Unlock()
	s.logSummary(false)
}

// logSummary logs the records dropped and failed since the last summary, if
// there are any and LogInterval has passed since it, or force is set.
func (s *Sink) logSummary(force bool) {
	s.logMu.Lock()
	now := time.Now()
	if s.pendDropped+s.pendFailed == 0 || !force && now.Sub(s.lastLog) < s.cfg.LogInterval {
		s.logMu.Unlock()
		return
	}
	dropped, failed, msg := s.pendDropped, s.pendFailed, s.lastErr
	s.pendDropped, s.pendFailed, s.lastErr, s.lastLog = 0, 0, "", now
	s.logMu.Unlock()

	// PHP's log_warning('Could not log API request log to firehose'), once
	// per interval rather than once per record.
	s.cfg.Logger.Warn("Could not log API request log to firehose",
		slog.Uint64("dropped", dropped),
		slog.Uint64("failed", failed),
		slog.String("message", msg),
	)
}
