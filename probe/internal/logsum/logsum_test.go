package logsum

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestOneLinePerKeyPerWindow(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	s := New()
	for i := 0; i < 10000; i++ {
		s.Inc("reject:origin")
	}
	s.Inc("accept:emfile")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, log, time.Hour); close(done) }()
	cancel() // final flush on exit
	<-done
	out := buf.String()
	if strings.Count(out, "reject:origin") != 1 || !strings.Contains(out, "count=10000") || strings.Count(out, "accept:emfile") != 1 {
		t.Fatalf("output:\n%s", out)
	}
	if len(s.Drain()) != 0 {
		t.Fatal("drained")
	}
}
