package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

const (
	chunkSize2MB = 2 * 1024 * 1024
	chunkSize4MB = 4 * 1024 * 1024
	chunkSize8MB = 8 * 1024 * 1024

	adaptiveHighSpeedThresholdMBs = 25.0
	adaptiveMaxSpeedThresholdMBs  = 50.0

	defaultHedgeStallTimeout  = 6 * time.Second
	defaultHedgeMinElapsed    = 4 * time.Second
	defaultHedgeCheckInterval = 250 * time.Millisecond
	defaultHedgeLowSpeedBps   = 150 * 1024 // 150 KB/s
	defaultHedgeBackoffDur    = 30 * time.Second
	defaultHedgeCooldown      = 3 * time.Second
	defaultHedgeWindowDur     = 15 * time.Second
	defaultMaxHedgesPerWindow = 2
)

var (
	bufferPoolsMu               sync.Mutex
	bufferPools                 = make(map[int]*sync.Pool)
	chunkPoolsMu                sync.Mutex
	chunkPools                  = make(map[int64]*sync.Pool)
	globalSpeedLimitBytesPerSec atomic.Int64
	globalRateLimiter           = rate.NewLimiter(rate.Inf, 2*1024*1024)
)

// setGlobalSpeedLimit dynamically updates the rate limiter and atomic byte counter.
// When limitMBs <= 0, bandwidth is unlimited. When > 0, rate.Limiter.SetLimit()
// is updated live without interrupting active streaming connections.
func setGlobalSpeedLimit(limitMBs int) {
	if limitMBs <= 0 {
		globalSpeedLimitBytesPerSec.Store(0)
		globalRateLimiter.SetLimit(rate.Inf)
		globalRateLimiter.SetBurst(2 * 1024 * 1024)
	} else {
		bps := int64(limitMBs) * 1024 * 1024
		globalSpeedLimitBytesPerSec.Store(bps)
		globalRateLimiter.SetLimit(rate.Limit(bps))
		burst := int(bps / 4)
		if burst < 512*1024 {
			burst = 512 * 1024
		}
		globalRateLimiter.SetBurst(burst)
	}
}

type rateLimitedReadCloser struct {
	ctx context.Context
	rc  io.ReadCloser
}

func (r *rateLimitedReadCloser) Read(p []byte) (int, error) {
	limit := globalSpeedLimitBytesPerSec.Load()
	if limit <= 0 {
		return r.rc.Read(p)
	}

	// Cap read slice size when throttling so telemetry updates stay smooth.
	maxChunk := 128 * 1024
	if len(p) > maxChunk {
		p = p[:maxChunk]
	}

	n, err := r.rc.Read(p)
	if n > 0 {
		if waitErr := globalRateLimiter.WaitN(r.ctx, n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

func (r *rateLimitedReadCloser) Close() error {
	return r.rc.Close()
}

type throttledTransport struct {
	base http.RoundTripper
}

func (t *throttledTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &rateLimitedReadCloser{
		ctx: req.Context(),
		rc:  resp.Body,
	}
	return resp, nil
}

// normalizeCopyBufferMB clamps the per-extractor io.CopyBuffer slice size so large
// prefetch settings (128-512 MB) do not allocate an oversized single read buffer.
func normalizeCopyBufferMB(sizeMB int) int {
	if runtime.GOOS == "android" && sizeMB > 4 {
		sizeMB = 4
	}
	switch {
	case sizeMB <= 0:
		return defaultBufferMB
	case sizeMB == 1, sizeMB == 2, sizeMB == 4, sizeMB == 8, sizeMB == 16:
		return sizeMB
	case sizeMB > 16:
		return 16
	default:
		return defaultBufferMB
	}
}

func acquireBuffer(sizeMB int) []byte {
	sizeMB = normalizeCopyBufferMB(sizeMB)

	bufferPoolsMu.Lock()
	pool, exists := bufferPools[sizeMB]
	if !exists {
		byteLen := sizeMB * 1024 * 1024
		pool = &sync.Pool{
			New: func() interface{} {
				return make([]byte, byteLen)
			},
		}
		bufferPools[sizeMB] = pool
	}
	bufferPoolsMu.Unlock()

	return pool.Get().([]byte)
}

func releaseBuffer(sizeMB int, buf []byte) {
	sizeMB = normalizeCopyBufferMB(sizeMB)

	bufferPoolsMu.Lock()
	pool, exists := bufferPools[sizeMB]
	bufferPoolsMu.Unlock()

	if exists && len(buf) == sizeMB*1024*1024 {
		pool.Put(buf)
	}
}

// chunkBucketSize rounds a chunk length up to the smallest standard pooled bucket (2MB, 4MB, or 8MB).
func chunkBucketSize(length int64) int64 {
	switch {
	case length <= chunkSize2MB:
		return chunkSize2MB
	case length <= chunkSize4MB:
		return chunkSize4MB
	case length <= chunkSize8MB:
		return chunkSize8MB
	default:
		return length
	}
}

// acquireChunkBuffer retrieves a reusable byte slice of length expectedLen from sync.Pool.
func acquireChunkBuffer(expectedLen int64) []byte {
	if expectedLen <= 0 {
		return nil
	}
	bucket := chunkBucketSize(expectedLen)
	if bucket != chunkSize2MB && bucket != chunkSize4MB && bucket != chunkSize8MB {
		return make([]byte, expectedLen)
	}

	chunkPoolsMu.Lock()
	pool, exists := chunkPools[bucket]
	if !exists {
		bSize := bucket
		pool = &sync.Pool{
			New: func() interface{} {
				return make([]byte, bSize)
			},
		}
		chunkPools[bucket] = pool
	}
	chunkPoolsMu.Unlock()

	raw := pool.Get().([]byte)
	if int64(cap(raw)) < expectedLen {
		return make([]byte, expectedLen)
	}
	return raw[:expectedLen]
}

// releaseChunkBuffer returns a chunk slice back to its corresponding sync.Pool bucket.
func releaseChunkBuffer(buf []byte) {
	if buf == nil {
		return
	}
	c := int64(cap(buf))
	if c != chunkSize2MB && c != chunkSize4MB && c != chunkSize8MB {
		return
	}

	chunkPoolsMu.Lock()
	pool, exists := chunkPools[c]
	chunkPoolsMu.Unlock()

	if exists {
		pool.Put(buf[:c])
	}
}

// buildStreamingTransport constructs a dedicated HTTP/1.1 transport with HTTP/2 disabled
// so each parallel worker uses its own TCP connection rather than multiplexing over a single socket.
func buildStreamingTransport(numWorkers int) *http.Transport {
	if numWorkers < 1 {
		numWorkers = 4
	}
	poolConns := numWorkers * 2

	dialer := &net.Dialer{
		Timeout:   20 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		MaxIdleConns:          poolConns,
		MaxIdleConnsPerHost:   poolConns,
		MaxConnsPerHost:       poolConns,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true,
		ReadBufferSize:        256 * 1024,
		WriteBufferSize:       64 * 1024,
	}
}

func createOptimizedHTTPClient(timeout time.Duration) *http.Client {
	return buildHTTPClientWithWorkers(timeout, false, 4)
}

func createStreamingHTTPClient(timeout time.Duration) *http.Client {
	return buildHTTPClientWithWorkers(timeout, true, 4)
}

func createStreamingHTTPClientWithWorkers(timeout time.Duration, numWorkers int) *http.Client {
	return buildHTTPClientWithWorkers(timeout, true, numWorkers)
}

func buildHTTPClient(timeout time.Duration, throttled bool) *http.Client {
	return buildHTTPClientWithWorkers(timeout, throttled, 4)
}

func buildHTTPClientWithWorkers(timeout time.Duration, throttled bool, numWorkers int) *http.Client {
	var transport http.RoundTripper = buildStreamingTransport(numWorkers)
	if throttled {
		transport = &throttledTransport{base: transport}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			scheme := strings.ToLower(req.URL.Scheme)
			if scheme != "http" && scheme != "https" {
				return fmt.Errorf("insecure redirect scheme %q: only http and https allowed", scheme)
			}
			return nil
		},
	}
}

func applyCustomHeaders(req *http.Request, headers map[string]string) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", defaultUserAgent)
	}
	for k, v := range headers {
		tk := strings.TrimSpace(k)
		tv := strings.TrimSpace(v)
		if tk != "" && tv != "" {
			req.Header.Set(tk, tv)
		}
	}
}

// validateArchiveURL validates that rawURL uses only http:// or https:// schemes.
// Multi-line URLs (multi-part archives) have each line validated.
func validateArchiveURL(rawURL string) error {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return errors.New("URL is required")
	}
	urls := parseURLList(trimmed)
	if len(urls) == 0 {
		return errors.New("URL is required")
	}
	for _, uStr := range urls {
		u, err := url.Parse(uStr)
		if err != nil {
			return fmt.Errorf("invalid archive URL %q: %w", uStr, err)
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "http" && scheme != "https" {
			return fmt.Errorf("insecure archive URL scheme %q: only http:// and https:// are supported", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("invalid archive URL %q: missing host", uStr)
		}
	}
	return nil
}

// probeRemoteURL probes a remote URL using HEAD, falling back to GET Range: bytes=0-0 on 405 Method Not Allowed or 403 Forbidden.
func probeRemoteURL(ctx context.Context, client *http.Client, targetURL string, headers map[string]string) (*http.Response, string, int64, error) {
	if err := validateArchiveURL(targetURL); err != nil {
		return nil, targetURL, 0, err
	}
	headReq, err := http.NewRequestWithContext(ctx, "HEAD", targetURL, nil)
	if err != nil {
		return nil, targetURL, 0, err
	}
	applyCustomHeaders(headReq, headers)

	resp, err := client.Do(headReq)
	if err == nil {
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
			resolved := targetURL
			if resp.Request != nil && resp.Request.URL != nil {
				resolved = resp.Request.URL.String()
			}
			return resp, resolved, resp.ContentLength, nil
		}
		// On 405 Method Not Allowed, 403 Forbidden, or other refusal of HEAD, cleanly fall back to GET Range: bytes=0-0
		if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 400 {
			_ = resp.Body.Close()
		} else {
			_ = resp.Body.Close()
			return nil, targetURL, 0, fmt.Errorf("archive server returned HTTP %s", resp.Status)
		}
	}

	getReq, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		return nil, targetURL, 0, err
	}
	applyCustomHeaders(getReq, headers)
	getReq.Header.Set("Range", "bytes=0-0")

	getResp, err := client.Do(getReq)
	if err != nil {
		return nil, targetURL, 0, err
	}

	resolved := targetURL
	if getResp.Request != nil && getResp.Request.URL != nil {
		resolved = getResp.Request.URL.String()
	}

	var totalLen int64 = getResp.ContentLength
	if getResp.StatusCode == http.StatusPartialContent {
		cr := getResp.Header.Get("Content-Range")
		if slashIdx := strings.LastIndex(cr, "/"); slashIdx != -1 {
			totalStr := strings.TrimSpace(cr[slashIdx+1:])
			if parsed, parseErr := strconv.ParseInt(totalStr, 10, 64); parseErr == nil && parsed > 0 {
				totalLen = parsed
			}
		}
	}

	return getResp, resolved, totalLen, nil
}

// computePrefetchWindowChunks calculates the platform-aware sliding-window prefetch capacity
// dynamically from the configured I/O stream buffer size and active chunk size.
func computePrefetchWindowChunks(goos string, bufferSizeMB int, chunkSize int64, workers int) int {
	if chunkSize <= 0 {
		chunkSize = chunkSize2MB
	}
	if workers < 1 {
		workers = 1
	}

	if goos == "android" {
		if bufferSizeMB > 64 {
			bufferSizeMB = 64
		}
		maxAndroidChunks := int((64 * 1024 * 1024) / chunkSize)
		if maxAndroidChunks < 4 {
			maxAndroidChunks = 4
		}
		var chunks int
		if bufferSizeMB <= defaultBufferMB {
			// Default conservative mobile prefetch cap: 4-6 chunks (~16-32 MB)
			chunks = 6
			if chunkSize >= chunkSize4MB {
				chunks = 4
			}
		} else {
			chunks = int((int64(bufferSizeMB) * 1024 * 1024) / chunkSize)
		}
		if chunks < 4 {
			chunks = 4
		}
		if chunks > maxAndroidChunks {
			chunks = maxAndroidChunks
		}
		return chunks
	}

	// Desktop (linux, windows, darwin)
	if bufferSizeMB > 512 {
		bufferSizeMB = 512
	}

	defaultDesktopWindow := 24
	if workers >= 12 {
		defaultDesktopWindow = 32
	}

	var chunks int
	if bufferSizeMB <= defaultBufferMB {
		chunks = defaultDesktopWindow
	} else {
		chunks = int((int64(bufferSizeMB) * 1024 * 1024) / chunkSize)
	}

	minFloor := defaultDesktopWindow
	if workers*2 > minFloor {
		minFloor = workers * 2
	}
	if chunks < minFloor {
		chunks = minFloor
	}
	return chunks
}

// nextAdaptiveChunkSize computes the target sequential chunk size (2MB -> 4MB -> 8MB on desktop,
// capped at 2MB -> 4MB on Android) based on sustained throughput and recent stall/hedge events.
func nextAdaptiveChunkSize(goos string, currentSize int64, sustainedSpeedMBs float64, recentHedges int) int64 {
	if currentSize <= 0 {
		currentSize = chunkSize2MB
	}
	maxChunk := int64(chunkSize8MB)
	if goos == "android" {
		maxChunk = int64(chunkSize4MB)
	}

	// Step down if stalls or hedges occur frequently
	if recentHedges >= 2 {
		var stepped int64
		if currentSize > chunkSize4MB {
			stepped = chunkSize4MB
		} else {
			stepped = chunkSize2MB
		}
		if stepped > maxChunk {
			stepped = maxChunk
		}
		return stepped
	}

	// Ramp up once sustained throughput exceeds 25 MB/s
	if sustainedSpeedMBs > adaptiveHighSpeedThresholdMBs && recentHedges == 0 {
		if currentSize < chunkSize4MB {
			return chunkSize4MB
		}
		if currentSize < maxChunk && sustainedSpeedMBs >= adaptiveMaxSpeedThresholdMBs {
			return maxChunk
		}
		if currentSize < maxChunk {
			return maxChunk
		}
	}

	if currentSize > maxChunk {
		return maxChunk
	}
	return currentSize
}

type chunkDescriptor struct {
	id    int64
	start int64
	end   int64
}

type inflightChunkState struct {
	id                   int64
	start                int64
	end                  int64
	startedAt            time.Time
	lastProgressUnixNano atomic.Int64
	bytesRead            atomic.Int64
	hedged               atomic.Bool
	done                 atomic.Bool
	hedgeTriggerCh       chan struct{}
}

type attemptOutcome struct {
	data       []byte
	err        error
	statusCode int
	isHedge    bool
}

type ParallelRangeStream struct {
	ctx               context.Context
	cancel            context.CancelFunc
	client            *http.Client
	url               string
	totalSize         int64
	workers           int
	bufferSizeMB      int
	goos              string
	chunkSize         int64
	readOffset        int64
	nextFetchID       int64
	nextFetchOffset   int64
	nextReadID        int64
	scheduledChunks   int64
	completedChunks   atomic.Int64
	bytesFetched      atomic.Int64
	currentBuf        []byte
	currentPos        int
	chunks            map[int64][]byte
	inflight          map[int64]*inflightChunkState
	mu                sync.Mutex
	cond              *sync.Cond
	fetchErr          error
	closed            bool
	headers           map[string]string
	onBytesRead       func(n int64)
	speedCalc         *rollingSpeedCalculator
	sustainedSpeedMBs float64
	recentHedges      int

	// Hedging configuration and state
	hedgeStallTimeout  time.Duration
	hedgeMinElapsed    time.Duration
	hedgeCheckInterval time.Duration
	hedgeLowSpeedBps   float64
	hedgeBackoffDur    time.Duration
	hedgeCooldown      time.Duration
	hedgeWindowDur     time.Duration
	maxHedgesPerWindow int
	hedgeBackoffUntil  time.Time
	lastHedgeTime      time.Time
	hedgeTimestamps    []time.Time
	lastHedgeSpeedMBs  float64
	unproductiveHedges int
	headWaitChunkID    int64
	headWaitSince      time.Time
	hedgeCount         atomic.Int64
}

func NewParallelRangeStream(ctx context.Context, client *http.Client, targetURL string, totalSize int64, workers int, headers map[string]string, onBytesRead func(n int64)) *ParallelRangeStream {
	return NewParallelRangeStreamWithBuffer(ctx, client, targetURL, totalSize, workers, defaultBufferMB, headers, onBytesRead)
}

func NewParallelRangeStreamWithBuffer(ctx context.Context, client *http.Client, targetURL string, totalSize int64, workers int, bufferSizeMB int, headers map[string]string, onBytesRead func(n int64)) *ParallelRangeStream {
	workers = normalizeParallelConnsForOS(runtime.GOOS, workers)
	bufferSizeMB = normalizeBufferMBForOS(runtime.GOOS, bufferSizeMB)

	streamCtx, streamCancel := context.WithCancel(ctx)
	prs := &ParallelRangeStream{
		ctx:                streamCtx,
		cancel:             streamCancel,
		client:             client,
		url:                targetURL,
		totalSize:          totalSize,
		workers:            workers,
		bufferSizeMB:       bufferSizeMB,
		goos:               runtime.GOOS,
		chunkSize:          chunkSize2MB,
		chunks:             make(map[int64][]byte),
		inflight:           make(map[int64]*inflightChunkState),
		headers:            headers,
		onBytesRead:        onBytesRead,
		speedCalc:          newRollingSpeedCalculator(),
		hedgeStallTimeout:  defaultHedgeStallTimeout,
		hedgeMinElapsed:    defaultHedgeMinElapsed,
		hedgeCheckInterval: defaultHedgeCheckInterval,
		hedgeLowSpeedBps:   defaultHedgeLowSpeedBps,
		hedgeBackoffDur:    defaultHedgeBackoffDur,
		hedgeCooldown:      defaultHedgeCooldown,
		hedgeWindowDur:     defaultHedgeWindowDur,
		maxHedgesPerWindow: defaultMaxHedgesPerWindow,
	}
	prs.cond = sync.NewCond(&prs.mu)
	prs.startWorkers()
	return prs
}

func (prs *ParallelRangeStream) startWorkers() {
	if prs.speedCalc != nil {
		prs.speedCalc.addSample(time.Now(), prs.bytesFetched.Load())
	}
	for i := 0; i < prs.workers; i++ {
		go prs.workerLoop()
	}
	go prs.headMonitorLoop()
}

func (prs *ParallelRangeStream) currentWindowCapLocked() int {
	return computePrefetchWindowChunks(prs.goos, prs.bufferSizeMB, prs.chunkSize, prs.workers)
}

// headMonitorLoop monitors the head chunk that the sequential archive reader is currently waiting on,
// triggering a hedged backup HTTP Range request only when the reader has been blocked waiting past the grace
// period and the head chunk is genuinely stalled (>= 6s zero progress) or below both 20% of per-stream average and 150 KB/s.
func (prs *ParallelRangeStream) headMonitorLoop() {
	prs.mu.Lock()
	interval := prs.hedgeCheckInterval
	prs.mu.Unlock()
	if interval <= 0 {
		interval = defaultHedgeCheckInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-prs.ctx.Done():
			return
		case now := <-ticker.C:
			prs.checkAndTriggerHeadHedge(now)
		}
	}
}

func (prs *ParallelRangeStream) checkAndTriggerHeadHedge(now time.Time) {
	prs.mu.Lock()
	if prs.closed || prs.fetchErr != nil {
		prs.mu.Unlock()
		return
	}

	totalFetched := prs.bytesFetched.Load()
	if prs.speedCalc != nil && totalFetched > 0 {
		if instSpeedMBs := prs.speedCalc.addSample(now, totalFetched); instSpeedMBs > 0 {
			if prs.sustainedSpeedMBs <= 0 {
				prs.sustainedSpeedMBs = instSpeedMBs
			} else {
				prs.sustainedSpeedMBs = 0.25*instSpeedMBs + 0.75*prs.sustainedSpeedMBs
			}
		}
	}

	if !prs.hedgeBackoffUntil.IsZero() && now.Before(prs.hedgeBackoffUntil) {
		prs.mu.Unlock()
		return
	}

	cooldown := prs.hedgeCooldown
	if cooldown <= 0 {
		cooldown = defaultHedgeCooldown
	}
	if !prs.lastHedgeTime.IsZero() && now.Sub(prs.lastHedgeTime) < cooldown {
		prs.mu.Unlock()
		return
	}

	windowDur := prs.hedgeWindowDur
	if windowDur <= 0 {
		windowDur = defaultHedgeWindowDur
	}
	maxPerWindow := prs.maxHedgesPerWindow
	if maxPerWindow <= 0 {
		maxPerWindow = defaultMaxHedgesPerWindow
	}
	cutoff := now.Add(-windowDur)
	validIdx := 0
	for validIdx < len(prs.hedgeTimestamps) && !prs.hedgeTimestamps[validIdx].After(cutoff) {
		validIdx++
	}
	if validIdx > 0 {
		prs.hedgeTimestamps = prs.hedgeTimestamps[validIdx:]
	}
	if len(prs.hedgeTimestamps) >= maxPerWindow {
		prs.mu.Unlock()
		return
	}

	headID := prs.nextReadID
	ic, exists := prs.inflight[headID]
	if !exists || ic == nil || ic.hedged.Load() || ic.done.Load() {
		prs.mu.Unlock()
		return
	}

	stallTimeout := prs.hedgeStallTimeout
	if stallTimeout <= 0 {
		stallTimeout = defaultHedgeStallTimeout
	}
	minElapsed := prs.hedgeMinElapsed
	if minElapsed <= 0 {
		minElapsed = defaultHedgeMinElapsed
	}
	lowSpeedBps := prs.hedgeLowSpeedBps
	if lowSpeedBps <= 0 {
		lowSpeedBps = defaultHedgeLowSpeedBps
	}

	activeWorkers := prs.workers
	if len(prs.inflight) > activeWorkers {
		activeWorkers = len(prs.inflight)
	}
	if activeWorkers < 1 {
		activeWorkers = 1
	}

	var headWaitElapsed time.Duration
	if prs.headWaitChunkID == headID && !prs.headWaitSince.IsZero() {
		headWaitElapsed = now.Sub(prs.headWaitSince)
	}

	rollingMBs := prs.sustainedSpeedMBs
	headBytes := ic.bytesRead.Load()
	otherStreamsHealthy := prs.completedChunks.Load() > 0 || len(prs.chunks) > 0 || rollingMBs > 0 || (totalFetched-headBytes) > 0
	prs.mu.Unlock()

	inFlightDur := now.Sub(ic.startedAt)
	lastProg := time.Unix(0, ic.lastProgressUnixNano.Load())
	noProgressDur := now.Sub(lastProg)

	// Genuine stall: chunk has been in-flight for >= stallTimeout (default 6s)
	// and has made zero byte progress for >= stallTimeout.
	stalledNoData := inFlightDur >= stallTimeout && noProgressDur >= stallTimeout

	// Degraded throughput: require the reader to have been blocked waiting on this head chunk
	// for at least minElapsed (default 4s) and the chunk to have been in-flight for >= minElapsed.
	// Compare against perStreamAvg = aggregateRollingThroughput / float64(activeWorkers),
	// and only treat as degraded if chunkSpeedBps < 20% of perStreamAvg AND < 150 KB/s.
	var degradedThroughput bool
	if headWaitElapsed >= minElapsed && inFlightDur >= minElapsed && otherStreamsHealthy {
		elapsedSec := inFlightDur.Seconds()
		if elapsedSec > 0 {
			chunkSpeedBps := float64(headBytes) / elapsedSec
			aggregateRollingThroughput := rollingMBs * 1024 * 1024
			if aggregateRollingThroughput <= 0 && totalFetched > 0 {
				aggregateRollingThroughput = float64(totalFetched) / elapsedSec
			}
			perStreamAvg := aggregateRollingThroughput / float64(activeWorkers)
			if perStreamAvg > 0 && chunkSpeedBps < 0.20*perStreamAvg && chunkSpeedBps < lowSpeedBps {
				degradedThroughput = true
			}
		}
	}

	if stalledNoData || degradedThroughput {
		if ic.hedged.CompareAndSwap(false, true) {
			prs.mu.Lock()
			prs.lastHedgeTime = now
			prs.hedgeTimestamps = append(prs.hedgeTimestamps, now)
			if prs.unproductiveHedges > 0 && (prs.lastHedgeSpeedMBs <= 0 || prs.sustainedSpeedMBs <= prs.lastHedgeSpeedMBs*1.10) {
				prs.unproductiveHedges++
			} else {
				prs.unproductiveHedges = 1
			}
			prs.lastHedgeSpeedMBs = prs.sustainedSpeedMBs

			// Automatic temporary backoff if hedges fire repeatedly without improving speed
			// (indicating server-wide throttling rather than a single slow socket).
			if prs.unproductiveHedges >= 2 {
				backoffDur := prs.hedgeBackoffDur
				if backoffDur <= 0 {
					backoffDur = defaultHedgeBackoffDur
				}
				until := now.Add(backoffDur)
				if until.After(prs.hedgeBackoffUntil) {
					prs.hedgeBackoffUntil = until
				}
			}

			// Only step down adaptive chunk size on genuine zero-progress stalls/timeouts,
			// so server-wide slowdowns do not get misclassified as hedge storms.
			if stalledNoData {
				prs.recentHedges++
				if prs.recentHedges >= 2 {
					prs.chunkSize = nextAdaptiveChunkSize(prs.goos, prs.chunkSize, prs.sustainedSpeedMBs, prs.recentHedges)
					prs.recentHedges = 1
				}
			}
			prs.mu.Unlock()

			select {
			case ic.hedgeTriggerCh <- struct{}{}:
			default:
			}
		}
	}
}

func (prs *ParallelRangeStream) claimNextChunkLocked() (chunkDescriptor, bool) {
	for !prs.closed && prs.fetchErr == nil {
		if prs.nextFetchOffset >= prs.totalSize {
			return chunkDescriptor{}, false
		}
		windowCap := prs.currentWindowCapLocked()
		activeWindow := int(prs.nextFetchID - prs.nextReadID)
		bufferedAndInflight := len(prs.chunks) + len(prs.inflight)
		if activeWindow < windowCap && bufferedAndInflight < windowCap {
			break
		}
		prs.cond.Wait()
	}
	if prs.closed || prs.fetchErr != nil || prs.ctx.Err() != nil || prs.nextFetchOffset >= prs.totalSize {
		return chunkDescriptor{}, false
	}

	// Keep initial wave at 2MB for fast time-to-first-byte; adapt size afterward.
	targetSize := int64(chunkSize2MB)
	if prs.scheduledChunks >= int64(prs.workers) {
		prs.chunkSize = nextAdaptiveChunkSize(prs.goos, prs.chunkSize, prs.sustainedSpeedMBs, prs.recentHedges)
		targetSize = prs.chunkSize
	}

	desc := chunkDescriptor{
		id:    prs.nextFetchID,
		start: prs.nextFetchOffset,
		end:   prs.nextFetchOffset + targetSize - 1,
	}
	if desc.end >= prs.totalSize {
		desc.end = prs.totalSize - 1
	}

	prs.nextFetchID++
	prs.nextFetchOffset = desc.end + 1
	prs.scheduledChunks++
	return desc, true
}

func (prs *ParallelRangeStream) workerLoop() {
	for {
		prs.mu.Lock()
		desc, ok := prs.claimNextChunkLocked()
		prs.mu.Unlock()
		if !ok {
			return
		}

		data, hedged, err := prs.fetchRangeWithRetry(desc)
		prs.mu.Lock()
		if err != nil {
			if prs.fetchErr == nil {
				prs.fetchErr = err
			}
			prs.cond.Broadcast()
			prs.mu.Unlock()
			return
		}
		if prs.closed {
			prs.mu.Unlock()
			releaseChunkBuffer(data)
			return
		}

		prs.chunks[desc.id] = data
		prs.completedChunks.Add(1)

		totalFetched := prs.bytesFetched.Load()
		if prs.speedCalc != nil {
			instSpeedMBs := prs.speedCalc.addSample(time.Now(), totalFetched)
			if instSpeedMBs > 0 {
				if prs.sustainedSpeedMBs <= 0 {
					prs.sustainedSpeedMBs = instSpeedMBs
				} else {
					prs.sustainedSpeedMBs = 0.25*instSpeedMBs + 0.75*prs.sustainedSpeedMBs
				}
			}
		}
		if !hedged {
			if prs.recentHedges > 0 {
				prs.recentHedges--
			}
			if prs.unproductiveHedges > 0 && prs.lastHedgeSpeedMBs > 0 && prs.sustainedSpeedMBs > prs.lastHedgeSpeedMBs*1.10 {
				prs.unproductiveHedges = 0
			}
		}

		prs.cond.Broadcast()
		prs.mu.Unlock()
	}
}

func (prs *ParallelRangeStream) recordRateLimitBackoff() {
	prs.mu.Lock()
	backoffDur := prs.hedgeBackoffDur
	if backoffDur <= 0 {
		backoffDur = defaultHedgeBackoffDur
	}
	until := time.Now().Add(backoffDur)
	if until.After(prs.hedgeBackoffUntil) {
		prs.hedgeBackoffUntil = until
	}
	prs.recentHedges += 2
	prs.chunkSize = nextAdaptiveChunkSize(prs.goos, prs.chunkSize, prs.sustainedSpeedMBs, prs.recentHedges)
	prs.recentHedges = 1
	prs.mu.Unlock()
}

func (prs *ParallelRangeStream) executeRangeAttempt(attemptCtx context.Context, ic *inflightChunkState, start, end int64, isHedge bool) attemptOutcome {
	prs.mu.Lock()
	targetURL := prs.url
	prs.mu.Unlock()

	req, err := http.NewRequestWithContext(attemptCtx, "GET", targetURL, nil)
	if err != nil {
		return attemptOutcome{err: err, isHedge: isHedge}
	}
	applyCustomHeaders(req, prs.headers)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	if isHedge {
		// Force hedged request onto a fresh connection
		req.Close = true
	}

	resp, err := prs.client.Do(req)
	if err != nil {
		return attemptOutcome{err: err, isHedge: isHedge}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		prs.recordRateLimitBackoff()
		return attemptOutcome{
			err:        fmt.Errorf("server rate limited (%s) for range %d-%d", resp.Status, start, end),
			statusCode: resp.StatusCode,
			isHedge:    isHedge,
		}
	}

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return attemptOutcome{
			err:        fmt.Errorf("unexpected HTTP status %s for range %d-%d", resp.Status, start, end),
			statusCode: resp.StatusCode,
			isHedge:    isHedge,
		}
	}

	expectedLen := (end - start) + 1
	buf := acquireChunkBuffer(expectedLen)
	var offset int64
	readStep := int64(64 * 1024)

	for offset < expectedLen {
		if attemptCtx.Err() != nil {
			releaseChunkBuffer(buf)
			return attemptOutcome{err: attemptCtx.Err(), isHedge: isHedge}
		}
		endSlice := offset + readStep
		if endSlice > expectedLen {
			endSlice = expectedLen
		}
		n, readErr := resp.Body.Read(buf[offset:endSlice])
		if n > 0 {
			offset += int64(n)
			nowNano := time.Now().UnixNano()
			ic.lastProgressUnixNano.Store(nowNano)
			for {
				prev := ic.bytesRead.Load()
				if offset <= prev {
					break
				}
				if ic.bytesRead.CompareAndSwap(prev, offset) {
					prs.bytesFetched.Add(offset - prev)
					break
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF && offset == expectedLen {
				break
			}
			releaseChunkBuffer(buf)
			return attemptOutcome{err: readErr, isHedge: isHedge}
		}
	}

	return attemptOutcome{data: buf, isHedge: isHedge, statusCode: resp.StatusCode}
}

func (prs *ParallelRangeStream) fetchRangeWithRetry(desc chunkDescriptor) ([]byte, bool, error) {
	start, end := desc.start, desc.end
	expectedLen := (end - start) + 1
	var lastErr error
	var wasHedged bool
	attempt := 1

	for attempt <= maxRetries {
		if prs.ctx.Err() != nil {
			return nil, wasHedged, prs.ctx.Err()
		}
		if attempt > 1 {
			manager.Update(func(s *ExtractionState) {
				s.RetryCount++
			})
			backoff := time.Duration(1<<(attempt-2)) * time.Second
			if backoff > 10*time.Second {
				backoff = 10 * time.Second
			}
			select {
			case <-prs.ctx.Done():
				return nil, wasHedged, prs.ctx.Err()
			case <-time.After(backoff + time.Duration(rand.Intn(300))*time.Millisecond):
			}
		}

		now := time.Now()
		ic := &inflightChunkState{
			id:             desc.id,
			start:          start,
			end:            end,
			startedAt:      now,
			hedgeTriggerCh: make(chan struct{}, 1),
		}
		ic.lastProgressUnixNano.Store(now.UnixNano())

		prs.mu.Lock()
		if prs.closed {
			prs.mu.Unlock()
			return nil, wasHedged, context.Canceled
		}
		prs.inflight[desc.id] = ic
		prs.mu.Unlock()

		attemptCtx, attemptCancel := context.WithCancel(prs.ctx)
		outcomesCh := make(chan attemptOutcome, 2)
		activeAttempts := 1

		go func() {
			outcomesCh <- prs.executeRangeAttempt(attemptCtx, ic, start, end, false)
		}()

		var winningData []byte
		var primaryStatus int
		var primaryErr error

		for activeAttempts > 0 {
			select {
			case <-ic.hedgeTriggerCh:
				if !ic.done.Load() && activeAttempts == 1 && attemptCtx.Err() == nil {
					wasHedged = true
					prs.hedgeCount.Add(1)
					activeAttempts++
					log.Printf("[STREAM:HEDGE] Spawning hedged backup Range request for head chunk %d (bytes=%d-%d)", desc.id, start, end)
					go func() {
						outcomesCh <- prs.executeRangeAttempt(attemptCtx, ic, start, end, true)
					}()
				}
			case out := <-outcomesCh:
				activeAttempts--
				if out.err == nil && out.data != nil {
					if winningData == nil {
						winningData = out.data
						ic.done.Store(true)
						// Immediately cancel the slower request's context so its body closes cleanly
						attemptCancel()
					} else {
						// Slower request finished around the same instant; recycle its buffer
						releaseChunkBuffer(out.data)
					}
				} else {
					if !out.isHedge {
						primaryErr = out.err
						primaryStatus = out.statusCode
					} else if primaryErr == nil {
						primaryErr = out.err
					}
				}
			}
		}

		attemptCancel()
		ic.done.Store(true)

		prs.mu.Lock()
		delete(prs.inflight, desc.id)
		targetURL := prs.url
		prs.mu.Unlock()

		if winningData != nil {
			for {
				prev := ic.bytesRead.Load()
				if expectedLen <= prev {
					break
				}
				if ic.bytesRead.CompareAndSwap(prev, expectedLen) {
					prs.bytesFetched.Add(expectedLen - prev)
					break
				}
			}
			return winningData, wasHedged, nil
		}

		if partial := ic.bytesRead.Swap(0); partial > 0 {
			prs.bytesFetched.Add(-partial)
		}

		hasProgress := prs.bytesFetched.Load() > 0
		if (primaryStatus == http.StatusForbidden || primaryStatus == http.StatusGone) && hasProgress {
			newURL, waitErr := manager.WaitForRefreshedURL(prs.ctx, targetURL, primaryStatus)
			if waitErr != nil {
				return nil, wasHedged, waitErr
			}
			prs.mu.Lock()
			prs.url = newURL
			prs.mu.Unlock()
			attempt = 1
			continue
		}

		lastErr = primaryErr
		attempt++
	}
	return nil, wasHedged, fmt.Errorf("chunk fetch failed after retries: %w", lastErr)
}

func (prs *ParallelRangeStream) Read(p []byte) (int, error) {
	if prs.ctx.Err() != nil {
		return 0, prs.ctx.Err()
	}

	if prs.currentPos >= len(prs.currentBuf) {
		if prs.currentBuf != nil {
			releaseChunkBuffer(prs.currentBuf)
			prs.currentBuf = nil
			prs.currentPos = 0
		}
		if prs.readOffset >= prs.totalSize {
			return 0, io.EOF
		}

		prs.mu.Lock()
		chunkID := prs.nextReadID
		for {
			if prs.closed || prs.ctx.Err() != nil {
				prs.headWaitSince = time.Time{}
				prs.mu.Unlock()
				return 0, context.Canceled
			}
			if data, ok := prs.chunks[chunkID]; ok {
				delete(prs.chunks, chunkID)
				prs.nextReadID++
				prs.headWaitChunkID = prs.nextReadID
				prs.headWaitSince = time.Time{}
				prs.currentBuf = data
				prs.currentPos = 0
				// Immediately notify waiting workers that a sliding-window slot is free
				prs.cond.Broadcast()
				break
			}
			if prs.fetchErr != nil {
				err := prs.fetchErr
				prs.headWaitSince = time.Time{}
				prs.mu.Unlock()
				return 0, err
			}
			if prs.headWaitChunkID != chunkID || prs.headWaitSince.IsZero() {
				prs.headWaitChunkID = chunkID
				prs.headWaitSince = time.Now()
			}
			prs.cond.Wait()
		}
		prs.mu.Unlock()
	}

	n := copy(p, prs.currentBuf[prs.currentPos:])
	prs.currentPos += n
	prs.readOffset += int64(n)
	if prs.onBytesRead != nil && n > 0 {
		prs.onBytesRead(int64(n))
	}
	return n, nil
}

func (prs *ParallelRangeStream) Close() error {
	prs.mu.Lock()
	prs.closed = true
	if prs.cancel != nil {
		prs.cancel()
	}
	for id, buf := range prs.chunks {
		releaseChunkBuffer(buf)
		delete(prs.chunks, id)
	}
	prs.chunks = nil
	if prs.currentBuf != nil {
		releaseChunkBuffer(prs.currentBuf)
		prs.currentBuf = nil
	}
	prs.cond.Broadcast()
	prs.mu.Unlock()
	return nil
}

type ResilientSingleStream struct {
	ctx           context.Context
	client        *http.Client
	resolvedURL   string
	currentOffset int64
	totalBytes    int64
	currentBody   io.ReadCloser
	headers       map[string]string
	onBytesRead   func(n int64)
}

func NewResilientSingleStream(ctx context.Context, client *http.Client, resolvedURL string, totalBytes int64, initialBody io.ReadCloser, headers map[string]string, onBytesRead func(n int64)) *ResilientSingleStream {
	manager.RegisterActiveBody(initialBody)
	return &ResilientSingleStream{
		ctx:         ctx,
		client:      client,
		resolvedURL: resolvedURL,
		totalBytes:  totalBytes,
		currentBody: initialBody,
		headers:     headers,
		onBytesRead: onBytesRead,
	}
}

func (rs *ResilientSingleStream) Read(p []byte) (int, error) {
	if rs.ctx.Err() != nil {
		return 0, rs.ctx.Err()
	}
	if rs.currentBody == nil {
		if err := rs.reconnect(); err != nil {
			return 0, err
		}
	}

	n, err := rs.currentBody.Read(p)
	if n > 0 {
		rs.currentOffset += int64(n)
		if rs.onBytesRead != nil {
			rs.onBytesRead(int64(n))
		}
	}

	if err != nil && err != io.EOF {
		if rs.ctx.Err() != nil {
			return n, rs.ctx.Err()
		}
		if recErr := rs.reconnect(); recErr != nil {
			return n, recErr
		}
		return rs.Read(p[n:])
	}
	return n, err
}

func (rs *ResilientSingleStream) reconnect() error {
	if rs.currentBody != nil {
		_ = rs.currentBody.Close()
		rs.currentBody = nil
	}

	var lastErr error
	attempt := 1
	skipBackoff := false
	for attempt <= maxRetries {
		if rs.ctx.Err() != nil {
			return rs.ctx.Err()
		}

		if !skipBackoff {
			backoffSec := 1 << (attempt - 1)
			if backoffSec > 15 {
				backoffSec = 15
			}
			jitter := time.Duration(rand.Intn(500)) * time.Millisecond

			manager.Update(func(s *ExtractionState) {
				s.RetryCount++
				s.Message = fmt.Sprintf("Reconnecting stream (attempt %d/%d in %ds)...", attempt, maxRetries, backoffSec)
			})

			select {
			case <-rs.ctx.Done():
				return rs.ctx.Err()
			case <-time.After(time.Duration(backoffSec)*time.Second + jitter):
			}
		}
		skipBackoff = false

		req, err := http.NewRequestWithContext(rs.ctx, "GET", rs.resolvedURL, nil)
		if err != nil {
			lastErr = err
			attempt++
			continue
		}
		applyCustomHeaders(req, rs.headers)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", rs.currentOffset))

		resp, err := rs.client.Do(req)
		if err != nil {
			lastErr = err
			attempt++
			continue
		}

		if resp.StatusCode == http.StatusPartialContent {
			rs.currentBody = resp.Body
			manager.RegisterActiveBody(resp.Body)
			if resp.Request != nil && resp.Request.URL != nil {
				rs.resolvedURL = resp.Request.URL.String()
			}
			manager.Update(func(s *ExtractionState) {
				s.Message = "Stream connection restored. Resuming extraction..."
			})
			return nil
		}

		_ = resp.Body.Close()
		if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone) && rs.currentOffset > 0 {
			newURL, waitErr := manager.WaitForRefreshedURL(rs.ctx, rs.resolvedURL, resp.StatusCode)
			if waitErr != nil {
				return waitErr
			}
			rs.resolvedURL = newURL
			attempt = 1
			skipBackoff = true
			continue
		}
		if resp.StatusCode == http.StatusOK && rs.currentOffset > 0 {
			return fmt.Errorf("host ignored Range resume header (HTTP 200)")
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			return fmt.Errorf("download link expired or range rejected (HTTP %s)", resp.Status)
		}
		lastErr = fmt.Errorf("unexpected HTTP status: %s", resp.Status)
		attempt++
	}
	return fmt.Errorf("network recovery failed after %d retries: %w", maxRetries, lastErr)
}

func (rs *ResilientSingleStream) Close() error {
	if rs.currentBody != nil {
		return rs.currentBody.Close()
	}
	return nil
}

type MultiPartStreamReader struct {
	ctx           context.Context
	client        *http.Client
	urls          []string
	parallelConns int
	bufferSizeMB  int
	headers       map[string]string
	currentIdx    int
	activeReader  io.ReadCloser
	bytesRead     atomic.Int64
	onBytesRead   func(n int64)
}

func NewMultiPartStreamReader(ctx context.Context, client *http.Client, urls []string, parallelConns int, headers map[string]string, onBytesRead func(n int64)) *MultiPartStreamReader {
	return NewMultiPartStreamReaderWithBuffer(ctx, client, urls, parallelConns, defaultBufferMB, headers, onBytesRead)
}

func NewMultiPartStreamReaderWithBuffer(ctx context.Context, client *http.Client, urls []string, parallelConns int, bufferSizeMB int, headers map[string]string, onBytesRead func(n int64)) *MultiPartStreamReader {
	return &MultiPartStreamReader{
		ctx:           ctx,
		client:        client,
		urls:          urls,
		parallelConns: parallelConns,
		bufferSizeMB:  bufferSizeMB,
		headers:       headers,
		onBytesRead:   onBytesRead,
	}
}

func (mp *MultiPartStreamReader) openNextPart() error {
	if mp.activeReader != nil {
		_ = mp.activeReader.Close()
		mp.activeReader = nil
	}
	if mp.currentIdx >= len(mp.urls) {
		return io.EOF
	}

	targetURL := mp.urls[mp.currentIdx]
	partNum := mp.currentIdx + 1
	totalParts := len(mp.urls)
	mp.currentIdx++

	manager.Update(func(s *ExtractionState) {
		s.CurrentPart = partNum
		s.TotalParts = totalParts
		if totalParts > 1 {
			s.Message = fmt.Sprintf("Streaming archive part %d of %d...", partNum, totalParts)
		}
	})

	for {
		req, err := http.NewRequestWithContext(mp.ctx, "GET", targetURL, nil)
		if err != nil {
			return err
		}
		applyCustomHeaders(req, mp.headers)

		resp, err := mp.client.Do(req)
		if err != nil {
			return err
		}
		if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone) && mp.bytesRead.Load() > 0 {
			_ = resp.Body.Close()
			newURL, waitErr := manager.WaitForRefreshedURL(mp.ctx, targetURL, resp.StatusCode)
			if waitErr != nil {
				return waitErr
			}
			targetURL = newURL
			continue
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			_ = resp.Body.Close()
			return fmt.Errorf("part %d returned HTTP %s", partNum, resp.Status)
		}

		resolvedURL := targetURL
		if resp.Request != nil && resp.Request.URL != nil {
			resolvedURL = resp.Request.URL.String()
		}

		acceptRanges := strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes")
		if mp.parallelConns > 1 && acceptRanges && resp.ContentLength > int64(prefetchChunkSz*2) {
			_ = resp.Body.Close()
			prs := NewParallelRangeStreamWithBuffer(mp.ctx, mp.client, resolvedURL, resp.ContentLength, mp.parallelConns, mp.bufferSizeMB, mp.headers, mp.onBytesRead)
			manager.RegisterActiveBody(prs)
			mp.activeReader = prs
			return nil
		}

		rss := NewResilientSingleStream(mp.ctx, mp.client, resolvedURL, resp.ContentLength, resp.Body, mp.headers, mp.onBytesRead)
		mp.activeReader = rss
		return nil
	}
}

func (mp *MultiPartStreamReader) Read(p []byte) (int, error) {
	for {
		if mp.ctx.Err() != nil {
			return 0, mp.ctx.Err()
		}
		if mp.activeReader == nil {
			if err := mp.openNextPart(); err != nil {
				return 0, err
			}
		}
		n, err := mp.activeReader.Read(p)
		if n > 0 {
			mp.bytesRead.Add(int64(n))
			return n, nil
		}
		if err == io.EOF {
			_ = mp.activeReader.Close()
			mp.activeReader = nil
			continue
		}
		return 0, err
	}
}

func (mp *MultiPartStreamReader) Close() error {
	if mp.activeReader != nil {
		return mp.activeReader.Close()
	}
	return nil
}
