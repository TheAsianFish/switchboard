package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"

	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/embed"
	"github.com/TheAsianFish/switchboard/internal/telemetry"
)

// Status is what a lookup found, as reported in the response header.
type Status string

// Lookup statuses.
const (
	// StatusSkip: the request was not cacheable or the cache was off.
	StatusSkip Status = "skip"
	// StatusMiss: cacheable, nothing matched; the reply will be stored.
	StatusMiss Status = "miss"
	// StatusExact and StatusSemantic: served from the cache.
	StatusExact    Status = "hit-exact"
	StatusSemantic Status = "hit-semantic"
	// StatusError: the store failed; the request went upstream and the
	// reply is not stored.
	StatusError Status = "error"
)

// Options configure a Cache.
type Options struct {
	// KV holds exact entries. Required.
	KV KV
	// NewIndex builds the vector index once the embedder's dimension is
	// known (on the first successful embedding). Nil, or a nil Embedder,
	// disables the semantic layer.
	NewIndex func(dims int) (Index, error)
	Embedder embed.Embedder
	// Threshold is the lowest cosine similarity served as a hit.
	Threshold float64
	// MatchNumbers refuses a semantic hit whose stored prompt does not
	// contain the same numbers as the new one.
	MatchNumbers bool
	// TTL is how long an entry lives.
	TTL time.Duration
	// Candidates is how many nearest entries a semantic lookup considers,
	// so that a near neighbour refused by the number check does not hide
	// a slightly further one that passes. Zero means 3.
	Candidates int
	// Timeout bounds each store and embedding call. Zero means 250 ms.
	Timeout time.Duration
	// RetryInterval is how long the cache stays bypassed after a store
	// error, so an unreachable Redis costs one timeout per interval and
	// not one per request; the semantic layer alone is bypassed as long
	// after an embedder or vector index error. Zero means one second.
	RetryInterval time.Duration
	// Metrics records lookups and fills; nil records nothing.
	Metrics *telemetry.Metrics
	Now     func() time.Time
}

// Cache is the two-layer response cache. It is safe for concurrent use.
type Cache struct {
	opts Options

	indexMu sync.Mutex
	index   Index
	dims    int

	// downUntil is the unix-nano time until which the store is bypassed,
	// embedDownUntil and indexDownUntil the same for the embedder and the
	// vector index, whose failures bypass the semantic layer only.
	downUntil      atomic.Int64
	embedDownUntil atomic.Int64
	indexDownUntil atomic.Int64
	// storeDown is set by a store failure and cleared, with a log line, by
	// the first store call that succeeds after it, so the end of an outage
	// is recorded as well as its start. storeFailures counts the failures:
	// a call that started before the latest one says nothing about the
	// store now, as in budget.Degrading.
	storeDown     atomic.Bool
	storeFailures atomic.Uint64

	// fillMu orders Fill's Add against Close's Wait: a WaitGroup must not
	// gain work while someone waits on it.
	fillMu sync.Mutex
	closed bool
	fills  sync.WaitGroup
}

// New builds a cache.
func New(opts Options) *Cache {
	if opts.Candidates <= 0 {
		opts.Candidates = 3
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 250 * time.Millisecond
	}
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Cache{opts: opts}
}

// Semantic reports whether the semantic layer is configured.
func (c *Cache) Semantic() bool { return c.opts.Embedder != nil && c.opts.NewIndex != nil }

// entry is the stored form of a reply.
type entry struct {
	// Prompt is the last user turn the reply answered; the number check
	// compares against it.
	Prompt     string          `json:"prompt"`
	Text       string          `json:"text"`
	StopReason core.StopReason `json:"stop_reason"`
	Input      int             `json:"input_tokens"`
	Output     int             `json:"output_tokens"`
	CacheRead  int             `json:"cache_read_tokens"`
	CacheWrite int             `json:"cache_write_tokens"`
	Created    int64           `json:"created"`
}

func (e entry) response() *core.Response {
	return &core.Response{
		Text: e.Text, StopReason: e.StopReason,
		Usage: core.Usage{InputTokens: e.Input, OutputTokens: e.Output, CacheReadTokens: e.CacheRead, CacheWriteTokens: e.CacheWrite},
	}
}

// Lookup is the outcome of looking a request up, and what is needed to
// store its reply on a miss.
type Lookup struct {
	Status Status
	// Similarity is the matched entry's cosine similarity, on a semantic
	// hit.
	Similarity float64
	// Response is the cached reply on a hit. Its ID is empty and its Model
	// unset: the caller fills in its own.
	Response *core.Response
	// Reason explains a skip or an error, for logs.
	Reason string

	keys Keys
	mode Mode
	// tools records that the request carried tool content (see Fill).
	tools bool
	// vec is the prompt's embedding, kept so a fill need not embed again.
	vec []float32
}

// Fillable reports whether the reply to this request should be stored.
func (l *Lookup) Fillable() bool { return l != nil && l.Status == StatusMiss }

// Skip returns a lookup that did not happen, with the reason.
func Skip(reason string) *Lookup { return &Lookup{Status: StatusSkip, Reason: reason} }

// Lookup looks req, routed to up, up for tenant in the given mode.
func (c *Cache) Lookup(ctx context.Context, tenant string, up Upstream, mode Mode, req *core.Request) *Lookup {
	if mode == ModeOff {
		return Skip("off")
	}
	keys, ok := KeysFor(tenant, up, req)
	if !ok {
		return Skip("last turn is not the user's")
	}
	if c.bypassed(&c.downUntil) {
		return c.record(&Lookup{Status: StatusSkip, Reason: "store unavailable"})
	}
	tools := req.HasToolContent()
	if tools && mode > ModeExact {
		mode = ModeExact // Decide says so too; this holds for any caller
	}
	l := &Lookup{Status: StatusMiss, keys: keys, mode: mode, tools: tools}

	start := c.opts.Now()
	wall := time.Now()
	seen := c.storeFailures.Load()
	sctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	raw, found, err := c.opts.KV.Get(sctx, exactKey(keys))
	cancel()
	waited := time.Since(wall)
	c.opts.Metrics.CacheLatency("exact", c.opts.Now().Sub(start))
	telemetry.StoreWaited(ctx, telemetry.StoreCacheLookup, "get", waited, err)
	if err != nil {
		c.storeFailed(ctx, "get", err, waited)
		return c.record(&Lookup{Status: StatusError, Reason: "store unavailable"})
	}
	c.storeAnswered(ctx, "get", seen)
	if found {
		if e, err := decode(raw); err == nil {
			l.Status, l.Response = StatusExact, e.response()
			return c.record(l)
		}
		// A corrupt entry is a miss; the fill overwrites it.
	}
	if mode < ModeSemantic || !c.Semantic() || c.bypassed(&c.embedDownUntil) || c.bypassed(&c.indexDownUntil) {
		return c.record(l)
	}
	c.semantic(ctx, l)
	return c.record(l)
}

// semantic embeds the prompt and looks for a stored reply to a reworded
// version of it in the same context. Failures leave l a miss: the exact
// layer still works without the embedder or the vector index.
func (c *Cache) semantic(ctx context.Context, l *Lookup) {
	start := c.opts.Now()
	defer func() { c.opts.Metrics.CacheLatency("semantic", c.opts.Now().Sub(start)) }()
	ectx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	vecs, err := c.opts.Embedder.Embed(ectx, []string{l.keys.Prompt})
	cancel()
	switch {
	case err == nil && len(vecs) == 1:
	case errors.Is(err, embed.ErrTooLong):
		// Longer than the model reads: matching on a truncated prompt
		// would serve one long prompt's reply for another that differs
		// only after the cut. Exact matching still applies.
		c.opts.Metrics.CacheError("embed_too_long")
		return
	case ctx.Err() != nil:
		return // the client went away; the embedder is fine
	default:
		c.opts.Metrics.CacheError("embed")
		c.embedDownUntil.Store(c.opts.Now().Add(c.opts.RetryInterval).UnixNano())
		telemetry.Logger(ctx).Warn("cache: embedding failed; semantic layer bypassed", slog.Any("error", err),
			slog.Duration("for", c.opts.RetryInterval))
		return
	}
	l.vec = vecs[0]
	idx, err := c.indexFor(len(l.vec))
	if err != nil {
		c.opts.Metrics.CacheError("index")
		telemetry.Logger(ctx).Warn("cache: no vector index", slog.String("error", err.Error()))
		l.vec = nil
		return
	}
	wall := time.Now()
	sctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	matches, err := idx.Search(sctx, Query{Namespace: l.keys.Namespace, Context: l.keys.Context, Vector: l.vec, K: c.opts.Candidates})
	cancel()
	telemetry.StoreWaited(ctx, telemetry.StoreCacheLookup, "search", time.Since(wall), err)
	if err != nil {
		c.indexFailed(ctx, "search", err)
		l.vec = nil
		return
	}
	for _, m := range matches {
		if m.Similarity < c.opts.Threshold {
			break // most similar first
		}
		e, err := decode(m.Payload)
		if err != nil {
			continue
		}
		if c.opts.MatchNumbers && !SameNumbers(e.Prompt, l.keys.Prompt) {
			continue
		}
		l.Status, l.Similarity, l.Response = StatusSemantic, m.Similarity, e.response()
		return
	}
}

// indexFor returns the vector index, building it on first use: its
// dimension is the embedder's, which is only known once it has answered.
func (c *Cache) indexFor(dims int) (Index, error) {
	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	if c.index != nil {
		if dims != c.dims {
			return nil, fmt.Errorf("%w: embedder returned %d, index has %d", ErrDimensions, dims, c.dims)
		}
		return c.index, nil
	}
	idx, err := c.opts.NewIndex(dims)
	if err != nil {
		return nil, err
	}
	c.index, c.dims = idx, dims
	return idx, nil
}

// Fill stores resp as the reply to the request l looked up, in the
// background: the client has its reply already and should not wait for
// the store. Replies cut short by a content filter, and empty ones, are
// not stored.
//
// Nor are replies with tool calls: an entry holds text only. A tool call
// is a step in the middle of a conversation whose next request (carrying
// the tool's result) is the one an exact hit can answer, and the streamed
// fill path sees the text of a stream, not its tool calls. For a request
// with tool content only a reply that ended on its own (end_turn or
// stop_sequence) is stored, so a stream cut off inside a tool call is
// never kept as its text alone.
func (c *Cache) Fill(ctx context.Context, l *Lookup, resp *core.Response) {
	if !l.Fillable() || resp == nil || resp.Text == "" || resp.StopReason == core.StopContentFilter {
		return
	}
	if len(resp.ToolCalls) > 0 || resp.StopReason == core.StopToolUse ||
		l.tools && resp.StopReason != core.StopEndTurn && resp.StopReason != core.StopSequence {
		return
	}
	e := entry{
		Prompt: l.keys.Prompt, Text: resp.Text, StopReason: resp.StopReason,
		Input: resp.Usage.InputTokens, Output: resp.Usage.OutputTokens,
		CacheRead: resp.Usage.CacheReadTokens, CacheWrite: resp.Usage.CacheWriteTokens,
		Created: c.opts.Now().Unix(),
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	c.fillMu.Lock()
	if c.closed {
		c.fillMu.Unlock()
		return
	}
	c.fills.Add(1)
	c.fillMu.Unlock()
	go func() {
		defer c.fills.Done()
		// A child of the request's span that may end after it: the
		// client has its reply by now.
		sctx, span := telemetry.StartSpan(ctx, "cache.fill", attribute.String("switchboard.cache.mode", l.mode.String()))
		var spanErr error
		defer func() { telemetry.EndSpan(span, spanErr) }()
		fctx, cancel := context.WithTimeout(sctx, c.opts.Timeout)
		defer cancel()
		seen := c.storeFailures.Load()
		wall := time.Now()
		err := c.opts.KV.Set(fctx, exactKey(l.keys), raw, c.opts.TTL)
		waited := time.Since(wall)
		telemetry.StoreWaited(ctx, telemetry.StoreCacheFill, "set", waited, err)
		if err != nil {
			spanErr = err
			c.opts.Metrics.CacheFill("error")
			c.storeFailed(ctx, "set", err, waited)
			return
		}
		c.storeAnswered(ctx, "set", seen)
		if l.mode == ModeSemantic && l.vec != nil {
			idx, err := c.indexFor(len(l.vec))
			if err == nil {
				// The exact key is the entry's id: refilling the same
				// prompt in the same context replaces, not duplicates.
				wall := time.Now()
				err = idx.Add(fctx, Item{ID: l.keys.Exact, Namespace: l.keys.Namespace, Context: l.keys.Context, Vector: l.vec, Payload: raw, TTL: c.opts.TTL})
				telemetry.StoreWaited(ctx, telemetry.StoreCacheFill, "add", time.Since(wall), err)
			}
			if err != nil {
				// The exact entry is stored; only the semantic one is
				// missing.
				spanErr = err
				c.opts.Metrics.CacheFill("error")
				c.indexFailed(ctx, "add", err)
				return
			}
		}
		c.opts.Metrics.CacheFill("ok")
	}()
}

// Wait blocks until the background fills started so far have finished.
// Tests use it; it must not run while Fill may be called concurrently,
// which is what Close is for.
func (c *Cache) Wait() { c.fills.Wait() }

// Close stops new fills and waits for those in flight, so the store can be
// closed after it. Lookups still work; replies are no longer stored.
func (c *Cache) Close() {
	c.fillMu.Lock()
	c.closed = true
	c.fillMu.Unlock()
	c.fills.Wait()
}

func (c *Cache) record(l *Lookup) *Lookup {
	c.opts.Metrics.CacheLookup(string(l.Status))
	return l
}

func (c *Cache) bypassed(until *atomic.Int64) bool {
	return c.opts.Now().UnixNano() < until.Load()
}

// storeFailed bypasses the cache for RetryInterval. A cache that waits out
// a store timeout on every request would make an outage slower than no
// cache at all. A call that failed because its own client went away says
// nothing about the store, and must not turn the cache off for everyone.
func (c *Cache) storeFailed(ctx context.Context, op string, err error, waited time.Duration) {
	if ctx.Err() != nil {
		return
	}
	c.opts.Metrics.CacheError(op)
	if errors.Is(err, ErrDimensions) {
		return
	}
	c.downUntil.Store(c.opts.Now().Add(c.opts.RetryInterval).UnixNano())
	c.storeFailures.Add(1)
	c.storeDown.Store(true)
	telemetry.Logger(ctx).Warn("cache store failed; bypassing the cache", slog.String("op", op),
		slog.String("error", err.Error()), slog.Duration("for", c.opts.RetryInterval),
		slog.Float64("wait_ms", telemetry.Millis(waited)))
}

// storeAnswered runs after a store call that succeeded; seen is the failure
// count when the call started. The first one after a failure logs that the
// store is back, at the failure's level, so a fleet logging at warn records
// both ends of every outage. A call that started before the latest failure
// (one in flight when the store went away) does not count: its success
// would announce a recovery in the middle of the outage.
func (c *Cache) storeAnswered(ctx context.Context, op string, seen uint64) {
	if c.storeFailures.Load() != seen {
		return
	}
	if c.storeDown.Load() && c.storeDown.CompareAndSwap(true, false) {
		telemetry.Logger(ctx).Warn("cache store reachable again", slog.String("op", op))
	}
}

// indexFailed bypasses the semantic layer for RetryInterval after a vector
// index error. The exact layer stays on: a store that answers Get and Set
// but not the vector commands (a Redis without the Query Engine) still
// serves exact hits.
func (c *Cache) indexFailed(ctx context.Context, op string, err error) {
	if ctx.Err() != nil {
		return
	}
	c.opts.Metrics.CacheError(op)
	if errors.Is(err, ErrDimensions) {
		return
	}
	c.indexDownUntil.Store(c.opts.Now().Add(c.opts.RetryInterval).UnixNano())
	telemetry.Logger(ctx).Warn("cache: vector index failed; semantic layer bypassed", slog.String("op", op),
		slog.String("error", err.Error()), slog.Duration("for", c.opts.RetryInterval))
}

func exactKey(k Keys) string { return "cache:x:" + k.Namespace + ":" + k.Exact }

func decode(raw []byte) (entry, error) {
	var e entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return entry{}, fmt.Errorf("cache: decode entry: %w", err)
	}
	return e, nil
}

// maxChunk bounds one replayed delta, in bytes.
const maxChunk = 64

// Chunks splits text into the deltas a cached reply is replayed as: one
// word with the whitespace after it, much like the token stream it came
// from, and never more than maxChunk bytes or a split UTF-8 sequence.
// Joined, the chunks are exactly text.
func Chunks(text string) []string {
	var out []string
	for len(text) > 0 {
		end := 0
		seenSpace := false
		for end < len(text) {
			r, size := utf8.DecodeRuneInString(text[end:])
			isSpace := strings.ContainsRune(" \t\n\r", r)
			if (seenSpace && !isSpace) || end+size > maxChunk {
				break
			}
			seenSpace = seenSpace || isSpace
			end += size
		}
		out = append(out, text[:end])
		text = text[end:]
	}
	return out
}
