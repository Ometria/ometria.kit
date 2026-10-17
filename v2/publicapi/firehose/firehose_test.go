package firehose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	"github.com/aws/aws-sdk-go-v2/service/firehose/types"

	"github.com/Ometria/ometria.kit/v2/publicapi"
)

type fakeClient struct {
	mu     sync.Mutex
	inputs []*firehose.PutRecordBatchInput
	err    error
	block  chan struct{} // If set, calls wait for it or their context.
	// fail decides whether a record fails in the call'th call (from 0).
	fail func(call int, data []byte) bool
}

func (f *fakeClient) PutRecordBatch(ctx context.Context, in *firehose.PutRecordBatchInput, _ ...func(*firehose.Options)) (*firehose.PutRecordBatchOutput, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	call := len(f.inputs)
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return nil, f.err
	}
	out := &firehose.PutRecordBatchOutput{}
	for _, r := range in.Records {
		entry := types.PutRecordBatchResponseEntry{RecordId: aws.String("id")}
		if f.fail != nil && f.fail(call, r.Data) {
			entry = types.PutRecordBatchResponseEntry{
				ErrorCode:    aws.String("ServiceUnavailableException"),
				ErrorMessage: aws.String("Slow down."),
			}
			out.FailedPutCount = aws.Int32(aws.ToInt32(out.FailedPutCount) + 1)
		}
		out.RequestResponses = append(out.RequestResponses, entry)
	}
	return out, nil
}

func (f *fakeClient) calls() []*firehose.PutRecordBatchInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*firehose.PutRecordBatchInput(nil), f.inputs...)
}

func testRecord(id string) publicapi.Record {
	return publicapi.Record{
		RequestID: id, Timestamp: time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC),
		APIKey: "WXYZ", AccountID: "42", Authenticated: true, Path: "/v2/push/_errors",
		RequestHTTPMethod: "GET", ResponseHTTPCode: 200,
	}
}

func TestSinkSends(t *testing.T) {
	client := &fakeClient{}
	s := New(client, Config{Stream: "api-log"})
	s.Send(context.Background(), testRecord("a"))
	s.Send(context.Background(), testRecord("b"))
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	calls := client.calls()
	if len(calls) != 1 || len(calls[0].Records) != 2 {
		t.Fatalf("expected 1 batch of 2 records, got %d batches", len(calls))
	}
	if aws.ToString(calls[0].DeliveryStreamName) != "api-log" {
		t.Errorf("stream = %q", aws.ToString(calls[0].DeliveryStreamName))
	}
	for _, r := range calls[0].Records {
		var row map[string]any
		if err := json.Unmarshal(r.Data, &row); err != nil {
			t.Fatal(err)
		}
		if row["account_id"] != float64(42) || row["path"] != "/v2/push/_errors" || row["response_error_message"] != nil {
			t.Errorf("unexpected row %s", r.Data)
		}
	}
	if st := s.Stats(); st != (Stats{Sent: 2}) {
		t.Errorf("stats = %+v", st)
	}
}

func TestSinkBatchesByCount(t *testing.T) {
	client := &fakeClient{}
	s := New(client, Config{Stream: "api-log", QueueSize: 2000, FlushInterval: time.Hour})
	for i := range 1100 {
		s.Send(context.Background(), testRecord(fmt.Sprint(i)))
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for _, c := range client.calls() {
		sizes = append(sizes, len(c.Records))
	}
	slices.Sort(sizes) // Workers send batches concurrently.
	if fmt.Sprint(sizes) != "[100 500 500]" {
		t.Errorf("batch sizes = %v", sizes)
	}
	if st := s.Stats(); st != (Stats{Sent: 1100}) {
		t.Errorf("stats = %+v", st)
	}
}

func TestSinkBatchesByBytes(t *testing.T) {
	client := &fakeClient{}
	s := New(client, Config{Stream: "api-log", FlushInterval: time.Hour})
	big := testRecord("big")
	big.RequestBody = strings.Repeat("x", 900<<10)
	for range 10 {
		s.Send(context.Background(), big)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := client.calls()
	if len(calls) < 3 {
		t.Fatalf("expected the records split over several batches, got %d", len(calls))
	}
	total := 0
	for _, c := range calls {
		size := 0
		for _, r := range c.Records {
			size += len(r.Data)
		}
		if size > MaxBatchBytes {
			t.Errorf("batch of %d bytes", size)
		}
		total += len(c.Records)
	}
	if total != 10 {
		t.Errorf("sent %d records", total)
	}
}

func TestSinkFailsOversizedRecord(t *testing.T) {
	client := &fakeClient{}
	s := New(client, Config{Stream: "api-log", Logger: slog.New(slog.DiscardHandler)})
	huge := testRecord("huge")
	huge.RequestBody = strings.Repeat("x", MaxRecordBytes)
	s.Send(context.Background(), huge)
	s.Send(context.Background(), testRecord("ok"))
	s.Close(context.Background()) //nolint:errcheck
	if st := s.Stats(); st != (Stats{Sent: 1, Failed: 1}) {
		t.Errorf("stats = %+v", st)
	}
}

func TestSinkFlushesAfterInterval(t *testing.T) {
	client := &fakeClient{}
	s := New(client, Config{Stream: "api-log", FlushInterval: 20 * time.Millisecond})
	defer s.Close(context.Background()) //nolint:errcheck
	s.Send(context.Background(), testRecord("a"))
	deadline := time.Now().Add(2 * time.Second)
	for len(client.calls()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("record not flushed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSinkRetriesFailedRecordsOnce(t *testing.T) {
	// "b" fails once, "c" always.
	client := &fakeClient{fail: func(call int, data []byte) bool {
		return bytes.Contains(data, []byte(`"request_id":"c"`)) || call == 0 && bytes.Contains(data, []byte(`"request_id":"b"`))
	}}
	logs := &bytes.Buffer{}
	s := New(client, Config{Stream: "api-log", Logger: slog.New(slog.NewTextHandler(logs, nil))})
	for _, id := range []string{"a", "b", "c"} {
		s.Send(context.Background(), testRecord(id))
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	calls := client.calls()
	if len(calls) != 2 || len(calls[0].Records) != 3 || len(calls[1].Records) != 2 {
		t.Fatalf("expected a batch of 3 then a retry of 2, got %d calls", len(calls))
	}
	if st := s.Stats(); st != (Stats{Sent: 2, Failed: 1}) {
		t.Errorf("stats = %+v", st)
	}
	if !strings.Contains(logs.String(), "ServiceUnavailableException") {
		t.Errorf("expected the error code in the log, got %s", logs.String())
	}
}

func TestSinkNeverBlocks(t *testing.T) {
	client := &fakeClient{block: make(chan struct{})}
	s := New(client, Config{
		Stream: "api-log", QueueSize: 1, Workers: 1, Timeout: 50 * time.Millisecond,
		FlushInterval: time.Millisecond, Logger: slog.New(slog.DiscardHandler),
	})

	start := time.Now()
	for i := range 2000 {
		s.Send(context.Background(), testRecord(fmt.Sprint(i)))
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("Send blocked for %s", d)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Dropped == 0 || st.Sent != 0 || st.Sent+st.Dropped+st.Failed != 2000 {
		t.Errorf("stats = %+v", st)
	}

	s.Send(context.Background(), testRecord("late"))
	if s.Stats().Dropped != st.Dropped+1 {
		t.Errorf("expected a send after Close to be dropped")
	}
}

func TestSinkRateLimitsLogs(t *testing.T) {
	logs := &bytes.Buffer{}
	s := New(&fakeClient{block: make(chan struct{})}, Config{
		Stream: "api-log", QueueSize: 1, Workers: 1, Timeout: 10 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
	})
	for i := range 1000 {
		s.Send(context.Background(), testRecord(fmt.Sprint(i)))
	}
	s.Close(context.Background()) //nolint:errcheck

	lines := strings.Count(logs.String(), "Could not log API request log to firehose")
	// One when the first drop happens, one summary at Close.
	if lines < 1 || lines > 2 {
		t.Errorf("expected at most 2 log lines, got %d:\n%s", lines, logs.String())
	}
	if !strings.Contains(logs.String(), "dropped=") || !strings.Contains(logs.String(), "queue full") {
		t.Errorf("expected a summary, got %s", logs.String())
	}
}

func TestSinkLogsErrors(t *testing.T) {
	logs := &bytes.Buffer{}
	s := New(&fakeClient{err: errors.New("throttled")}, Config{Stream: "api-log", Logger: slog.New(slog.NewTextHandler(logs, nil))})
	s.Send(context.Background(), testRecord("a"))
	s.Close(context.Background()) //nolint:errcheck
	if !strings.Contains(logs.String(), "throttled") {
		t.Errorf("expected the error to be logged, got %s", logs.String())
	}
	if st := s.Stats(); st != (Stats{Failed: 1}) {
		t.Errorf("stats = %+v", st)
	}
}

func TestCloseTimeoutStopsWorkers(t *testing.T) {
	before := runtime.NumGoroutine()

	client := &fakeClient{block: make(chan struct{})} // Never unblocks.
	s := New(client, Config{
		Stream: "api-log", Timeout: time.Hour, FlushInterval: time.Millisecond,
		Logger: slog.New(slog.DiscardHandler),
	})
	for i := range 10 {
		s.Send(context.Background(), testRecord(fmt.Sprint(i)))
	}
	time.Sleep(20 * time.Millisecond) // Let a send start.

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want deadline exceeded", err)
	}

	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("workers still running after Close timed out")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before New, %d after Close", before, runtime.NumGoroutine())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st := s.Stats(); st.Sent != 0 || st.Failed != 10 {
		t.Errorf("stats = %+v", st)
	}
}

func TestNewClient(t *testing.T) {
	c := NewClient(aws.Config{})
	if c.Options().Region != DefaultRegion {
		t.Errorf("region = %q", c.Options().Region)
	}
	c = NewClient(aws.Config{Region: "us-east-1"})
	if c.Options().Region != "us-east-1" {
		t.Errorf("region = %q", c.Options().Region)
	}
}
