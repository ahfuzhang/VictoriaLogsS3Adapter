package ratelimit

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

func TestWriterLimitsRate(t *testing.T) {
	const bps = 512 * 1024
	data := bytes.Repeat([]byte("x"), 2*bps) // first burst is free, so expect >= ~1.5s
	var dst bytes.Buffer
	w := NewWriter(context.Background(), &dst, bps)
	start := time.Now()
	n, err := w.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if n != len(data) || !bytes.Equal(dst.Bytes(), data) {
		t.Fatalf("data mismatch: wrote %d of %d", n, len(data))
	}
	if elapsed < 1200*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("unexpected elapsed time %s for %d bytes at %d B/s", elapsed, len(data), bps)
	}
}

func TestReaderLimitsRate(t *testing.T) {
	const bps = 512 * 1024
	data := bytes.Repeat([]byte("y"), 2*bps)
	r := NewReader(context.Background(), bytes.NewReader(data), bps)
	start := time.Now()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if !bytes.Equal(got, data) {
		t.Fatal("data mismatch")
	}
	if elapsed < 1200*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("unexpected elapsed time %s", elapsed)
	}
}

func TestUnlimited(t *testing.T) {
	data := bytes.Repeat([]byte("z"), 8<<20)
	var dst bytes.Buffer
	start := time.Now()
	if _, err := NewWriter(context.Background(), &dst, 0).Write(data); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("unlimited writer is too slow")
	}
}

func TestWriterCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := NewWriter(ctx, io.Discard, 1024)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := w.Write(make([]byte, 1<<20))
	if err == nil {
		t.Fatal("expected error after cancel")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancel did not interrupt the wait")
	}
}
