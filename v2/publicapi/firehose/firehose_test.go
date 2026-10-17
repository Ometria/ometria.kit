package firehose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/firehose"

	"github.com/Ometria/ometria.kit/v2/publicapi"
)

type fakeClient struct {
	mu     sync.Mutex
	inputs []*firehose.PutRecordInput
	err    error
	block  chan struct{}
}

func (f *fakeClient) PutRecord(ctx context.Context, in *firehose.PutRecordInput, _ ...func(*firehose.Options)) (*firehose.PutRecordOutput, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, in)
	return &firehose.PutRecordOutput{}, f.err
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

	if len(client.inputs) != 2 {
		t.Fatalf("expected 2 records, got %d", len(client.inputs))
	}
	for _, in := range client.inputs {
		if aws.ToString(in.DeliveryStreamName) != "api-log" {
			t.Errorf("stream = %q", aws.ToString(in.DeliveryStreamName))
		}
		var row map[string]any
		if err := json.Unmarshal(in.Record.Data, &row); err != nil {
			t.Fatal(err)
		}
		if row["account_id"] != float64(42) || row["path"] != "/v2/push/_errors" || row["response_error_message"] != nil {
			t.Errorf("unexpected row %s", in.Record.Data)
		}
	}
}

func TestSinkNeverBlocks(t *testing.T) {
	logs := &bytes.Buffer{}
	client := &fakeClient{block: make(chan struct{})}
	s := New(client, Config{
		Stream: "api-log", QueueSize: 1, Workers: 1, Timeout: 50 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
	})

	start := time.Now()
	for i := range 5 {
		s.Send(context.Background(), testRecord(string(rune('a'+i))))
	}
	if d := time.Since(start); d > 20*time.Millisecond {
		t.Errorf("Send blocked for %s", d)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "queue full") || !strings.Contains(logs.String(), "deadline exceeded") {
		t.Errorf("expected dropped records to be logged, got %s", logs.String())
	}

	s.Send(context.Background(), testRecord("late"))
	if !strings.Contains(logs.String(), "sink closed") {
		t.Errorf("expected a send after Close to be dropped, got %s", logs.String())
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
