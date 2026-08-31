// Package metrics exposes Prometheus metrics for the engine.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"crossvenue/internal/domain"
)

// Metrics is the engine's metric surface.
type Metrics struct {
	MarketEvents     *prometheus.CounterVec
	SequenceGaps     *prometheus.CounterVec
	BookResyncs      *prometheus.CounterVec
	WSReconnects     *prometheus.CounterVec
	BookAge          *prometheus.GaugeVec
	QueueDepth       *prometheus.GaugeVec
	Opportunities    prometheus.Counter
	OppsRejected     *prometheus.CounterVec
	Orders           prometheus.Counter
	Fills            prometheus.Counter
	PartialFills     prometheus.Counter
	RiskRejections   *prometheus.CounterVec
	ProcLatency      *prometheus.HistogramVec
	ExecLatency      *prometheus.HistogramVec
	PositionNotional *prometheus.GaugeVec
	SimulatedPnL     prometheus.Gauge
	EventsDropped    *prometheus.CounterVec
}

// New registers all metrics.
func New(reg prometheus.Registerer) *Metrics {
	f := promauto.With(reg)
	l := []string{"venue"}
	return &Metrics{
		MarketEvents: f.NewCounterVec(prometheus.CounterOpts{
			Name: "crossvenue_market_events_total"}, append(l, "symbol", "type")),
		SequenceGaps: f.NewCounterVec(prometheus.CounterOpts{
			Name: "crossvenue_sequence_gaps_total"}, l),
		BookResyncs: f.NewCounterVec(prometheus.CounterOpts{
			Name: "crossvenue_book_resync_total"}, l),
		WSReconnects: f.NewCounterVec(prometheus.CounterOpts{
			Name: "crossvenue_ws_reconnect_total"}, l),
		BookAge: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "crossvenue_book_age_seconds"}, append(l, "symbol")),
		QueueDepth: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "crossvenue_event_queue_depth"}, l),
		Opportunities: f.NewCounter(prometheus.CounterOpts{
			Name: "crossvenue_opportunities_total"}),
		OppsRejected: f.NewCounterVec(prometheus.CounterOpts{
			Name: "crossvenue_opportunities_rejected_total"}, []string{"reason"}),
		Orders: f.NewCounter(prometheus.CounterOpts{
			Name: "crossvenue_orders_total"}),
		Fills: f.NewCounter(prometheus.CounterOpts{
			Name: "crossvenue_fills_total"}),
		PartialFills: f.NewCounter(prometheus.CounterOpts{
			Name: "crossvenue_partial_fills_total"}),
		RiskRejections: f.NewCounterVec(prometheus.CounterOpts{
			Name: "crossvenue_risk_rejections_total"}, []string{"reason"}),
		ProcLatency: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "crossvenue_processing_latency_seconds",
			Buckets: prometheus.ExponentialBuckets(0.00005, 2, 14)}, append(l, "stage")),
		ExecLatency: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "crossvenue_execution_latency_seconds",
			Buckets: prometheus.ExponentialBuckets(0.0001, 2, 14)}, l),
		PositionNotional: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "crossvenue_position_notional"}, append(l, "symbol")),
		SimulatedPnL: f.NewGauge(prometheus.GaugeOpts{
			Name: "crossvenue_simulated_pnl"}),
		EventsDropped: f.NewCounterVec(prometheus.CounterOpts{
			Name: "crossvenue_events_dropped_total"}, l),
	}
}

// IncSequenceGap implements marketdata.Metrics.
func (m *Metrics) IncSequenceGap(v domain.Venue) { m.SequenceGaps.WithLabelValues(string(v)).Inc() }

// IncResync implements marketdata.Metrics.
func (m *Metrics) IncResync(v domain.Venue) { m.BookResyncs.WithLabelValues(string(v)).Inc() }

// IncDropped implements marketdata.Metrics.
func (m *Metrics) IncDropped(v domain.Venue) { m.EventsDropped.WithLabelValues(string(v)).Inc() }

// SetQueueDepth implements marketdata.Metrics.
func (m *Metrics) SetQueueDepth(v domain.Venue, depth int) {
	m.QueueDepth.WithLabelValues(string(v)).Set(float64(depth))
}
