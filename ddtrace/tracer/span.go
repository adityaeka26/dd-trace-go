package tracer

import (
	"fmt"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/ext"
)

var _ ddtrace.Span = (*span)(nil)

// span represents a computation. Callers must call Finish when a span is
// complete to ensure it's submitted.
type span struct {
	Name     string             `json:"name"`              // operation name
	Service  string             `json:"service"`           // service name (i.e. "grpc.server", "http.request")
	Resource string             `json:"resource"`          // resource name (i.e. "/user?id=123", "SELECT * FROM users")
	Type     string             `json:"type"`              // protocol associated with the span (i.e. "web", "db", "cache")
	Start    int64              `json:"start"`             // span start time expressed in nanoseconds since epoch
	Duration int64              `json:"duration"`          // duration of the span expressed in nanoseconds
	Meta     map[string]string  `json:"meta,omitempty"`    // arbitrary map of metadata
	Metrics  map[string]float64 `json:"metrics,omitempty"` // arbitrary map of numeric metrics
	SpanID   uint64             `json:"span_id"`           // identifier of this span
	TraceID  uint64             `json:"trace_id"`          // identifier of the root span
	ParentID uint64             `json:"parent_id"`         // identifier of the span's direct parent
	Error    int32              `json:"error"`             // error status of the span; 0 means no errors

	sync.RWMutex
	finished bool // true if the span has been submitted to a tracer.

	// parent contains a link to the parent. In most cases, ParentID can be inferred from this.
	// However, ParentID can technically be overridden (typical usage: distributed tracing)
	// and also, parent == nil is used to identify root and top-level ("local root") spans.
	parent  *span
	context *spanContext
}

// Context yields the SpanContext for this Span. Note that the return
// value of Context() is still valid after a call to Finish(). This is
// called the span context and it is different from Go's context.
func (s *span) Context() ddtrace.SpanContext { return s.context }

// SetBaggageItem sets a key/value pair as baggage on the span. Baggage items
// are propagated down to descendant spans and injected cross-process. Use with
// care as it adds extra load onto your tracing layer.
func (s *span) SetBaggageItem(key, val string) {
	s.context.setBaggageItem(key, val)
}

// BaggageItem gets the value for a baggage item given its key. Returns the
// empty string if the value isn't found in this Span.
func (s *span) BaggageItem(key string) string {
	return s.context.baggageItem(key)
}

// SetTag adds a set of key/value metadata to the span.
func (s *span) SetTag(key string, value interface{}) {
	s.Lock()
	defer s.Unlock()
	// We don't lock spans when flushing, so we could have a data race when
	// modifying a span as it's being flushed. This protects us against that
	// race, since spans are marked `finished` before we flush them.
	if s.finished {
		return
	}
	if key == ext.Error {
		s.setTagError(value)
		return
	}
	if v, ok := value.(string); ok {
		s.setTagString(key, v)
		return
	}
	if v, ok := toFloat64(value); ok {
		s.setTagNumeric(key, v)
		return
	}
	// not numeric, not a string and not an error, the likelihood of this
	// happening is close to zero, but we should nevertheless account for it.
	s.Meta[key] = fmt.Sprint(value)
}

// setTagError sets the error tag. It accounts for various valid scenarios.
// This method is not safe for concurrent use.
func (s *span) setTagError(value interface{}) {
	switch v := value.(type) {
	case bool:
		// bool value as per Opentracing spec.
		if !v {
			s.Error = 0
		} else {
			s.Error = 1
		}
	case error:
		// if anyone sets an error value as the tag, be nice here
		// and provide all the benefits.
		s.Error = 1
		s.Meta[ext.ErrorMsg] = v.Error()
		s.Meta[ext.ErrorType] = reflect.TypeOf(v).String()
		s.Meta[ext.ErrorStack] = string(debug.Stack())
	case nil:
		// no error
		s.Error = 0
	default:
		// in all other cases, let's assume that setting this tag
		// is the result of an error.
		s.Error = 1
	}
}

// setTagString sets a string tag. This method is not safe for concurrent use.
func (s *span) setTagString(key, v string) {
	switch key {
	case ext.ServiceName:
		s.Service = v
	case ext.ResourceName:
		s.Resource = v
	case ext.SpanType:
		s.Type = v
	default:
		s.Meta[key] = v
	}
}

// setTagNumeric sets a numeric tag, in our case called a metric. This method
// is not safe for concurrent use.
func (s *span) setTagNumeric(key string, v float64) {
	switch key {
	case ext.SamplingPriority:
		// setting sampling priority per spec
		s.Metrics[samplingPriorityKey] = v
	default:
		s.Metrics[key] = v
	}
}

// Finish closes this Span (but not its children) providing the duration
// of its part of the tracing session.
func (s *span) Finish(opts ...ddtrace.FinishOption) {
	var cfg ddtrace.FinishConfig
	for _, fn := range opts {
		fn(&cfg)
	}
	var t int64
	if cfg.FinishTime.IsZero() {
		t = now()
	} else {
		t = cfg.FinishTime.UnixNano()
	}
	if cfg.Error != nil {
		s.SetTag(ext.Error, cfg.Error)
	}
	s.finish(t)
}

// SetOperationName sets or changes the operation name.
func (s *span) SetOperationName(operationName string) {
	s.Lock()
	defer s.Unlock()

	s.Name = operationName
}

func (s *span) finish(finishTime int64) {
	s.Lock()
	defer s.Unlock()
	// We don't lock spans when flushing, so we could have a data race when
	// modifying a span as it's being flushed. This protects us against that
	// race, since spans are marked `finished` before we flush them.
	if s.finished {
		// already finished
		return
	}
	if s.Duration == 0 {
		s.Duration = finishTime - s.Start
	}
	s.finished = true

	if !s.context.sampled {
		// not sampled
		return
	}
	s.context.finish()
}

// String returns a human readable representation of the span. Not for
// production, just debugging.
func (s *span) String() string {
	lines := []string{
		fmt.Sprintf("Name: %s", s.Name),
		fmt.Sprintf("Service: %s", s.Service),
		fmt.Sprintf("Resource: %s", s.Resource),
		fmt.Sprintf("TraceID: %d", s.TraceID),
		fmt.Sprintf("SpanID: %d", s.SpanID),
		fmt.Sprintf("ParentID: %d", s.ParentID),
		fmt.Sprintf("Start: %s", time.Unix(0, s.Start)),
		fmt.Sprintf("Duration: %s", time.Duration(s.Duration)),
		fmt.Sprintf("Error: %d", s.Error),
		fmt.Sprintf("Type: %s", s.Type),
		"Tags:",
	}
	s.RLock()
	for key, val := range s.Meta {
		lines = append(lines, fmt.Sprintf("\t%s:%s", key, val))
	}
	s.RUnlock()
	return strings.Join(lines, "\n")
}

const samplingPriorityKey = "_sampling_priority_v1"
