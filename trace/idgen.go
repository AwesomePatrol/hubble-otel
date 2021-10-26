package trace

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"os"
	"strconv"
	"time"

	badger "github.com/dgraph-io/badger/v3"
	"github.com/isovalent/hubble-otel/common"

	"go.opentelemetry.io/contrib/propagators/aws/xray"
	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/contrib/propagators/jaeger"
	"go.opentelemetry.io/contrib/propagators/ot"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/cilium/cilium/api/v1/flow"
)

type TraceCache struct {
	MaxTraceLength time.Duration
	Strict         bool

	badgerDB *badger.DB
	logger   badger.Logger
}

type IDTuple struct {
	TraceID trace.TraceID
	SpanID  trace.SpanID
}

type keyTuple [2]string

func (kt keyTuple) isValid() bool { return kt != (keyTuple{}) }

type entryHelper struct {
	keys              keyTuple
	flowData          *bytes.Buffer
	spanHash          hash.Hash64
	traceHash         hash.Hash
	spanContext       trace.SpanContext
	linkedSpanContext trace.SpanContext
	httpHeaders       *propagation.HeaderCarrier
}

func newEntry() *entryHelper {
	return &entryHelper{
		keys:      keyTuple{},
		spanHash:  fnv.New64a(),
		traceHash: fnv.New128a(),
	}
}

func (e *entryHelper) checkHeaders(f *flow.Flow) error {
	if f.GetL7() == nil {
		return nil
	}
	http := f.L7.GetHttp()
	if http == nil {
		return nil
	}
	headers := http.GetHeaders()
	if headers == nil {
		return nil
	}

	ctx := propagators.Extract(context.Background(), e.makeHeaderCarrier(headers))

	e.spanContext = trace.SpanFromContext(ctx).SpanContext()
	// link attributes can only be populated explicitly as optional arguments
	// to LinkFromContext, since none are passed here, only context is kept
	e.linkedSpanContext = trace.LinkFromContext(ctx).SpanContext

	return nil
}

func (e *entryHelper) makeHeaderCarrier(headers []*flow.HTTPHeader) *propagation.HeaderCarrier {
	hc := &propagation.HeaderCarrier{}
	for _, header := range headers {
		hc.Set(header.Key, header.Value)
	}
	return hc
}

var propagators = propagation.NewCompositeTextMapPropagator(
	b3.New(),
	&jaeger.Jaeger{},
	&ot.OT{},
	&xray.Propagator{},
)

func (e *entryHelper) processFlowData(log badger.Logger, f *flow.Flow, strict bool) error {
	if err := e.checkHeaders(f); err != nil {
		return err
	}

	e.flowData = bytes.NewBuffer([]byte{})

	writers := []io.Writer{e.flowData}

	if !(e.spanContext.HasSpanID() && e.spanContext.HasTraceID()) {
		kt := e.generateKeys(f)
		if !kt.isValid() {
			// skip flows where keyTuple cannot be generated
			log.Debugf("flow has invalid key tuple: %+v", f)
			if strict {
				return fmt.Errorf("invalid key tuple: %+v", f)
			}
			return nil
		}
		e.keys = kt

		writers = append(writers, e.spanHash, e.traceHash)
	}

	flowData, err := common.MarshalJSON(f)
	if err != nil {
		return err
	}

	_, err = io.MultiWriter(writers...).Write(flowData)
	return err
}

func (e *entryHelper) generateSpanID(scc *trace.SpanContextConfig) {
	_ = e.spanHash.Sum(scc.SpanID[:0])
}

func (e *entryHelper) generateTraceID(scc *trace.SpanContextConfig) {
	// ensure trace prefix is a timestamp for AWS XRay compatibility
	binary.BigEndian.PutUint32(scc.TraceID[:4], uint32(time.Now().Unix()))
	// remaining bytes contain the first 12 bytes of FNV hash that fit
	fullHash := trace.TraceID{}
	_ = e.traceHash.Sum(fullHash[:0])
	copy(scc.TraceID[4:], fullHash[:])
}

func (e *entryHelper) generateKeys(f *flow.Flow) keyTuple {
	var src, dst string

	if ip := f.GetIP(); ip != nil {
		switch ip.GetIpVersion() {
		case flow.IPVersion_IPv4:
			src = ip.Source
			dst = ip.Destination
		case flow.IPVersion_IPv6:
			src = "[" + ip.Source + "]"
			dst = "[" + ip.Destination + "]"
		}
	}

	haveL4 := false
	if tcp := f.L4.GetTCP(); tcp != nil {
		src += ":" + strconv.Itoa(int(tcp.SourcePort))
		dst += ":" + strconv.Itoa(int(tcp.DestinationPort))
		haveL4 = true
	}
	if udp := f.L4.GetUDP(); udp != nil {
		src += ":" + strconv.Itoa(int(udp.SourcePort))
		dst += ":" + strconv.Itoa(int(udp.DestinationPort))
		haveL4 = true
	}
	if icmp := f.L4.GetICMPv4(); icmp != nil {
		src += "/" + strconv.Itoa(int(icmp.Type))
		haveL4 = true
	}
	if icmp := f.L4.GetICMPv6(); icmp != nil {
		src += "/" + strconv.Itoa(int(icmp.Type))
		haveL4 = true
	}

	if !haveL4 || src == "" || dst == "" {
		return keyTuple{}
	}

	src += "|" + strconv.Itoa(int(f.Source.Identity))
	dst += "|" + strconv.Itoa(int(f.Destination.Identity))

	return keyTuple{src + "<=>" + dst, dst + "<=>" + src}
}

func (e *entryHelper) flowDataKey(i int) string {
	return e.keys[i] + "/flowdData"
}

func (e *entryHelper) traceIDKey(i int) string {
	return e.keys[i] + "/traceID"
}

func (e *entryHelper) fetchTraceID(txn *badger.Txn, updateTTL time.Duration) (trace.TraceID, error) {
	traceID := trace.TraceID{}
	for i := range e.keys {
		key := []byte(e.traceIDKey(i))
		item, err := txn.Get(key)
		switch err {
		case nil:
			err := item.Value(func(val []byte) error {
				if len(val) != len(traceID) && !traceID.IsValid() {
					return fmt.Errorf("stored trace ID is invlaid")
				}
				copy(traceID[:], val)
				entry := badger.NewEntry(key, val).WithTTL(updateTTL)
				if err := txn.SetEntry(entry); err != nil {
					return fmt.Errorf("unable to update TTL for %q: %w", key, err)
				}
				return nil
			})
			if err != nil {
				return trace.TraceID{}, err
			}
		case badger.ErrKeyNotFound:
			continue
		default:
			return trace.TraceID{}, fmt.Errorf("unexpected error getting trace ID: %w", err)
		}
	}
	return traceID, nil
}

func (tc *TraceCache) storeTraceID(txn *badger.Txn, e *entryHelper, traceID trace.TraceID) error {
	data := map[string][]byte{
		e.traceIDKey(0):  traceID[:],
		e.flowDataKey(0): e.flowData.Bytes(),
	}
	if err := tc.storeKeys(txn, data); err != nil {
		return fmt.Errorf("unable to store newly generated trace ID: %w", err)
	}
	return nil
}

func NewTraceCache(opt badger.Options) (*TraceCache, error) {
	db, err := badger.Open(opt)
	if err != nil {
		return nil, err
	}
	return &TraceCache{
		MaxTraceLength: 20 * time.Minute,
		badgerDB:       db,
		logger:         opt.Logger,
	}, nil
}

func (tc *TraceCache) GetSpanContext(f *flow.Flow) (*trace.SpanContext, *trace.SpanContext, error) {
	e := newEntry()

	if err := e.processFlowData(tc.logger, f, tc.Strict); err != nil {
		return nil, nil, fmt.Errorf("unable to serialise flow: %w", err)
	}

	if e.spanContext.HasSpanID() && e.spanContext.HasTraceID() {
		if e.linkedSpanContext.HasSpanID() && e.linkedSpanContext.HasTraceID() {
			return &e.spanContext, &e.linkedSpanContext, nil
		}
		return &e.spanContext, nil, nil
	}

	scc := &trace.SpanContextConfig{}

	e.generateSpanID(scc) // always generate new span ID

	err := tc.badgerDB.Update(func(txn *badger.Txn) error {
		fetchedTraceID, err := e.fetchTraceID(txn, tc.MaxTraceLength)
		if err != nil {
			return fmt.Errorf("unable to get span/trace ID: %w", err)
		}
		// when both keys are missing, an empty (i.e. invalid) value is returned
		if !fetchedTraceID.IsValid() {
			e.generateTraceID(scc)
			return tc.storeTraceID(txn, e, scc.TraceID)
		}

		scc.TraceID = fetchedTraceID
		return nil
	})

	if err != nil {
		return nil, nil, err
	}

	e.spanContext = trace.NewSpanContext(*scc)
	return &e.spanContext, nil, nil
}

func (tc *TraceCache) Close() error {
	return tc.badgerDB.Close()
}

func (tc *TraceCache) Delete() {
	_ = tc.Close()
	os.RemoveAll(tc.badgerDB.Opts().Dir)
}

func (tc *TraceCache) storeKeys(txn *badger.Txn, data map[string][]byte) error {
	for k, v := range data {
		entry := badger.NewEntry([]byte(k), v).WithTTL(tc.MaxTraceLength)
		if err := txn.SetEntry(entry); err != nil {
			return fmt.Errorf("unable to store %q: %w", k, err)
		}
	}
	return nil
}