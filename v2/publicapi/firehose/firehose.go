// Package firehose sends publicapi's request log to an Amazon Data Firehose
// stream, as PHP's ometria.api does when API_LOG_FIREHOSE_STREAM is set.
//
// Each publicapi.Record becomes one PutRecord whose data is the record's
// JSON, shaped like PHP's json_encode of its row. Records are queued and
// sent in the background, so the request never waits for Firehose and a
// failure to log never fails it; records that can't be queued or sent are
// dropped with a warning.
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
	"log/slog"
	"net"
	"sync"
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

// PutRecordAPI is the part of *firehose.Client a Sink uses.
type PutRecordAPI interface {
	PutRecord(ctx context.Context, in *firehose.PutRecordInput, optFns ...func(*firehose.Options)) (*firehose.PutRecordOutput, error)
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
	// Timeout bounds each PutRecord call, retries included. Defaults to 2s.
	Timeout time.Duration
	// QueueSize is how many records can wait to be sent before new ones are
	// dropped. Defaults to 1000.
	QueueSize int
	// Workers is how many records are sent at once. Defaults to 4.
	Workers int
	// Logger receives a warning for every record dropped. Defaults to
	// slog.Default().
	Logger *slog.Logger
}

// Sink is a publicapi.Sink that sends records to Firehose.
type Sink struct {
	client PutRecordAPI
	cfg    Config
	queue  chan publicapi.Record

	mu     sync.RWMutex
	closed bool
	wg     sync.WaitGroup
}

var _ publicapi.Sink = (*Sink)(nil)

// New returns a Sink sending to client and starts its workers. Call Close
// to send the queued records and stop them.
func New(client PutRecordAPI, cfg Config) *Sink {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1000
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	s := &Sink{client: client, cfg: cfg, queue: make(chan publicapi.Record, cfg.QueueSize)}
	s.wg.Add(cfg.Workers)
	for range cfg.Workers {
		go s.work()
	}
	return s
}

// Send queues rec without blocking. It drops rec if the queue is full or
// the Sink is closed.
func (s *Sink) Send(ctx context.Context, rec publicapi.Record) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.drop(ctx, rec, errors.New("sink closed"))
		return
	}
	select {
	case s.queue <- rec:
	default:
		s.drop(ctx, rec, errors.New("queue full"))
	}
}

// Close stops accepting records and waits until the queued ones are sent or
// ctx is done.
func (s *Sink) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Sink) work() {
	defer s.wg.Done()
	for rec := range s.queue {
		s.put(rec)
	}
}

func (s *Sink) put(rec publicapi.Record) {
	data, err := json.Marshal(rec)
	if err != nil {
		s.drop(context.Background(), rec, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()
	_, err = s.client.PutRecord(ctx, &firehose.PutRecordInput{
		DeliveryStreamName: aws.String(s.cfg.Stream),
		Record:             &types.Record{Data: data},
	})
	if err != nil {
		s.drop(ctx, rec, err)
	}
}

func (s *Sink) drop(ctx context.Context, rec publicapi.Record, err error) {
	// PHP's log_warning('Could not log API request log to firehose').
	s.cfg.Logger.WarnContext(ctx, "Could not log API request log to firehose",
		slog.String("request_id", rec.RequestID),
		slog.String("account_id", rec.AccountID),
		slog.String("message", err.Error()),
	)
}
