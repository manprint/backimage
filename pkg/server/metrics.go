package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// Metrics contains lock-free counters shared by all sessions.
type Metrics struct {
	active        atomic.Int64
	received      atomic.Uint64
	uploaded      atomic.Uint64
	skipped       atomic.Uint64
	errorsGeneric atomic.Uint64
	errorsUsage   atomic.Uint64
	errorsAuth    atomic.Uint64
	errorsData    atomic.Uint64
	errorsNetwork atomic.Uint64
	durationNS    atomic.Uint64
	sessions      atomic.Uint64
}

func (m *Metrics) sessionStarted() { m.active.Add(1) }
func (m *Metrics) sessionDone(d time.Duration) {
	m.active.Add(-1)
	m.sessions.Add(1)
	m.durationNS.Add(uint64(d))
}
func (m *Metrics) addReceived(n int)    { m.received.Add(uint64(n)) }
func (m *Metrics) addUploaded(n uint64) { m.uploaded.Add(n) }
func (m *Metrics) addSkipped()          { m.skipped.Add(1) }
func (m *Metrics) addError(kind uint32) {
	switch kind {
	case ErrorUsage:
		m.errorsUsage.Add(1)
	case ErrorAuth:
		m.errorsAuth.Add(1)
	case ErrorIntegrity:
		m.errorsData.Add(1)
	case ErrorNetwork:
		m.errorsNetwork.Add(1)
	default:
		m.errorsGeneric.Add(1)
	}
}

// Handler exposes /healthz and dependency-free Prometheus text metrics.
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok\n")); err != nil {
			return
		}
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		write := func(format string, args ...any) bool {
			_, err := fmt.Fprintf(w, format, args...)
			return err == nil
		}
		if !write("backimage_sessions_active %d\n", m.active.Load()) ||
			!write("backimage_sessions_total %d\n", m.sessions.Load()) ||
			!write("backimage_bytes_received_total %d\n", m.received.Load()) ||
			!write("backimage_bytes_uploaded_total %d\n", m.uploaded.Load()) ||
			!write("backimage_layers_skipped_total %d\n", m.skipped.Load()) ||
			!write("backimage_session_duration_seconds_sum %.9f\n", float64(m.durationNS.Load())/float64(time.Second)) ||
			!write("backimage_errors_total{kind=\"generic\"} %d\n", m.errorsGeneric.Load()) ||
			!write("backimage_errors_total{kind=\"usage\"} %d\n", m.errorsUsage.Load()) ||
			!write("backimage_errors_total{kind=\"auth\"} %d\n", m.errorsAuth.Load()) ||
			!write("backimage_errors_total{kind=\"integrity\"} %d\n", m.errorsData.Load()) ||
			!write("backimage_errors_total{kind=\"network\"} %d\n", m.errorsNetwork.Load()) {
			return
		}
	})
	return mux
}
