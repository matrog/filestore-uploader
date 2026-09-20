package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// retryBackoff is multiplied by the attempt number: 20s, then 40s.
const retryBackoff = 20 * time.Second

// ErrInterrupted marks a job stopped by Ctrl-C rather than by a real failure.
var ErrInterrupted = errors.New("interrupted before completion")

type JobState int32

const (
	StateQueued JobState = iota
	StateRunning
	StateDone
	StateFailed
)

// Job is a single file to upload. The counters are atomic because the renderer
// reads them while the workers update them.
type Job struct {
	Path string
	Name string
	Size int64

	transferred atomic.Int64
	state       atomic.Int32

	mu       sync.Mutex
	link     string
	code     string
	err      error
	started  time.Time
	ended    time.Time
	attempt  int
	retryMsg string
	placed   bool
}

// MarkPlaced records that the file has already been moved into the destination,
// so the final pass does not ask the server to move it a second time.
func (j *Job) MarkPlaced() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.placed = true
}

func (j *Job) Placed() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.placed
}

// noteRetry records why an attempt failed, so the display can show it instead
// of silently restarting the progress bar from zero.
func (j *Job) noteRetry(attempt int, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.attempt = attempt
	if err != nil {
		j.retryMsg = err.Error()
	}
}

func (j *Job) RetryInfo() (attempt int, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.attempt, j.retryMsg
}

func (j *Job) State() JobState     { return JobState(j.state.Load()) }
func (j *Job) Transferred() int64  { return j.transferred.Load() }
func (j *Job) setState(s JobState) { j.state.Store(int32(s)) }

func (j *Job) Result() (link, code string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.link, j.code, j.err
}

func (j *Job) Elapsed() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.started.IsZero() {
		return 0
	}
	if j.ended.IsZero() {
		return time.Since(j.started)
	}
	return j.ended.Sub(j.started)
}

// countingReader counts the file's bytes as they reach the socket.
type countingReader struct {
	r io.Reader
	j *Job
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.j.transferred.Add(int64(n))
	}
	return n, err
}

// limitRe reads the ceiling out of the server's own error message, e.g.
// "Max filesize limit exceeded! Filesize limit: 10 Mb".
var limitRe = regexp.MustCompile(`(?i)filesize limit:?\s*([0-9.]+)\s*([kmg])b`)

// parseServerLimit converts that message into bytes, or 0 when absent.
func parseServerLimit(msg string) int64 {
	m := limitRe.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	switch strings.ToLower(m[2]) {
	case "k":
		return int64(n * (1 << 10))
	case "m":
		return int64(n * (1 << 20))
	case "g":
		return int64(n * (1 << 30))
	}
	return 0
}

// Uploader runs the jobs through a worker pool.
type Uploader struct {
	client  *Client
	workers int
	retries int

	// sizeLimit is learned from the first rejection and then applied up front,
	// so the remaining files fail in milliseconds instead of after sending
	// gigabytes the server was always going to refuse.
	sizeLimit atomic.Int64

	// OnSuccess fires as soon as the server confirms a file, so state is recorded
	// immediately rather than only when the whole transfer ends.
	OnSuccess func(j *Job, link, code string)

	// OnRetry fires before a new attempt, carrying the reason the previous one
	// failed. Without it a retry looks like the file restarting for no reason.
	OnRetry func(j *Job, attempt int, err error)

	// OnVerified fires when a failed attempt turns out to have landed anyway.
	OnVerified func(j *Job, link, code string)

	// OnLimit fires the first time the server reveals a maximum file size.
	OnLimit func(limit int64)

	// utype tells upload.cgi which account tier to apply. Omitting it makes the
	// server treat the upload as anonymous, and the anonymous tier caps files
	// at 10 MB — which is why large uploads failed while small ones worked.
	utype string
}

// SetUserType sets the account tier sent with each upload ("prem" or "reg").
func (u *Uploader) SetUserType(t string) {
	if t != "" {
		u.utype = t
	}
}

// SetSizeLimit applies a known limit before the run starts.
func (u *Uploader) SetSizeLimit(n int64) { u.sizeLimit.Store(n) }

// alreadyLanded asks the server whether the file arrived despite the error.
// A false answer is always safe: the worst case is the upload we were going to
// do anyway.
func (u *Uploader) alreadyLanded(j *Job) (link, code string, ok bool) {
	files, err := u.client.FindRecent(j.Name, 120)
	if err != nil {
		return "", "", false
	}
	for _, f := range files {
		if f.FileCode == "" || !strings.EqualFold(f.Name, j.Name) {
			continue
		}
		// When the server reports a size, it must match: a truncated upload
		// must not be mistaken for a complete one.
		if sz := strings.TrimSpace(f.Size.String()); sz != "" {
			n, convErr := strconv.ParseInt(sz, 10, 64)
			if convErr == nil && n != j.Size {
				continue
			}
		}
		return u.client.cfg.Site + "/" + f.FileCode, f.FileCode, true
	}
	return "", "", false
}

func NewUploader(c *Client, workers, retries int) *Uploader {
	if workers < 1 {
		workers = 1
	}
	if retries < 0 {
		retries = 0
	}
	return &Uploader{client: c, workers: workers, retries: retries, utype: "reg"}
}

// Run uploads every job and returns once the last one is finished.
func (u *Uploader) Run(ctx context.Context, jobs []*Job) {
	queue := make(chan *Job)
	var wg sync.WaitGroup

	for i := 0; i < u.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				u.runOne(ctx, j)
			}
		}()
	}

	go func() {
		defer close(queue)
		for _, j := range jobs {
			select {
			case <-ctx.Done():
				return
			case queue <- j:
			}
		}
	}()

	wg.Wait()
}

func (u *Uploader) runOne(ctx context.Context, j *Job) {
	if limit := u.sizeLimit.Load(); limit > 0 && j.Size > limit {
		j.mu.Lock()
		j.err = fmt.Errorf("%s exceeds the server limit of %s per file",
			humanBytes(j.Size), humanBytes(limit))
		j.ended = time.Now()
		j.mu.Unlock()
		j.setState(StateFailed)
		return
	}

	j.mu.Lock()
	j.started = time.Now()
	j.mu.Unlock()
	j.setState(StateRunning)

	var lastErr error
	for attempt := 0; attempt <= u.retries; attempt++ {
		if attempt > 0 {
			// A retry re-sends the whole file: make the reason visible, because
			// on a multi-gigabyte upload this costs many minutes.
			j.noteRetry(attempt, lastErr)
			if u.OnRetry != nil {
				u.OnRetry(j, attempt, lastErr)
			}
			// A generous pause: the failures seen in practice are the upload
			// server's own internal callback timing out, and re-sending
			// gigabytes two seconds later just hits a server still in trouble.
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
			case <-time.After(time.Duration(attempt) * retryBackoff):
			}
			j.transferred.Store(0)
		}
		link, code, err := u.uploadOnce(ctx, j)
		if err == nil {
			j.mu.Lock()
			j.link, j.code, j.ended = link, code, time.Now()
			j.mu.Unlock()
			j.setState(StateDone)
			if u.OnSuccess != nil {
				u.OnSuccess(j, link, code)
			}
			return
		}
		lastErr = err
		if limit := parseServerLimit(err.Error()); limit > 0 {
			u.sizeLimit.Store(limit)
			if u.OnLimit != nil {
				u.OnLimit(limit)
			}
		}
		if ctx.Err() != nil || !retryable(err) {
			break
		}

		// The connection may have died after the server got the whole file.
		// Ask before re-sending gigabytes: it also prevents duplicates.
		if link, code, ok := u.alreadyLanded(j); ok {
			j.mu.Lock()
			j.link, j.code, j.ended = link, code, time.Now()
			j.mu.Unlock()
			j.setState(StateDone)
			if u.OnVerified != nil {
				u.OnVerified(j, link, code)
			}
			if u.OnSuccess != nil {
				u.OnSuccess(j, link, code)
			}
			return
		}
	}

	// A cancelled context is the user pressing Ctrl-C, not a network fault:
	// reporting it as one is misleading.
	if ctx.Err() != nil {
		lastErr = ErrInterrupted
	}

	j.mu.Lock()
	j.err, j.ended = lastErr, time.Now()
	j.mu.Unlock()
	j.setState(StateFailed)
}

// retryable separates network hiccups from application-level rejections.
func retryable(err error) bool {
	s := strings.ToLower(err.Error())
	for _, hard := range []string{
		"not enabled", "invalid key", "invalid session", "quota", "too large",
		"filesize limit", "max filesize", "limit exceeded", "not allowed",
	} {
		if strings.Contains(s, hard) {
			return false
		}
	}
	return true
}

func (u *Uploader) uploadOnce(ctx context.Context, j *Job) (link, code string, err error) {
	target, err := u.client.UploadServer()
	if err != nil {
		return "", "", err
	}

	f, err := os.Open(j.Path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return "", "", err
	}
	size := st.Size()

	// The multipart body is assembled by hand because its exact length must be
	// known up front. With io.Pipe Go would use chunked encoding, which CGI
	// scripts such as upload.cgi often reject.
	boundary, err := randomBoundary()
	if err != nil {
		return "", "", err
	}
	var head bytes.Buffer
	writeMultipartField(&head, boundary, "sess_id", target.SessID)
	if u.utype != "" {
		writeMultipartField(&head, boundary, "utype", u.utype)
	}
	fmt.Fprintf(&head, "--%s\r\n", boundary)
	fmt.Fprintf(&head, "Content-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n",
		escapeQuotes(filepath.Base(j.Name)))
	fmt.Fprintf(&head, "Content-Type: application/octet-stream\r\n\r\n")
	tail := fmt.Sprintf("\r\n--%s--\r\n", boundary)

	body := io.MultiReader(
		bytes.NewReader(head.Bytes()),
		&countingReader{r: f, j: j},
		strings.NewReader(tail),
	)

	// The browser posts to upload.cgi?upload_type=file&utype=prem — the tier
	// lives in the QUERY STRING. Sent only as a form field it is ignored, and
	// the server falls back to the registered tier with its 100 MB ceiling.
	postURL := target.URL
	q := url.Values{}
	if !strings.Contains(postURL, "upload_type=") {
		q.Set("upload_type", "file")
	}
	if u.utype != "" && !strings.Contains(postURL, "utype=") {
		q.Set("utype", u.utype)
	}
	if len(q) > 0 {
		sep := "?"
		if strings.Contains(postURL, "?") {
			sep = "&"
		}
		postURL += sep + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, postURL, body)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.ContentLength = int64(head.Len()) + size + int64(len(tail))

	resp, err := u.client.hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("network error during upload: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("HTTP %d from the upload server: %s", resp.StatusCode, snippet(respBody))
	}
	return parseUploadResponse(respBody, u.client.cfg.Site)
}

func randomBoundary() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "filestoreboundary" + hex.EncodeToString(buf[:]), nil
}

func writeMultipartField(b *bytes.Buffer, boundary, name, value string) {
	fmt.Fprintf(b, "--%s\r\n", boundary)
	fmt.Fprintf(b, "Content-Disposition: form-data; name=\"%s\"\r\n\r\n", escapeQuotes(name))
	fmt.Fprintf(b, "%s\r\n", value)
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"", "\r", "", "\n", "")

func escapeQuotes(s string) string { return quoteEscaper.Replace(s) }

var fileCodeRe = regexp.MustCompile(`file_code["'\s:=]+([0-9a-zA-Z]{6,30})`)

type uploadItem struct {
	FileCode   string `json:"file_code"`
	FileStatus string `json:"file_status"`
	FileName   string `json:"file_name"`
}

func parseUploadResponse(body []byte, site string) (link, code string, err error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", "", fmt.Errorf("the upload server returned nothing")
	}

	var items []uploadItem
	if e := json.Unmarshal([]byte(trimmed), &items); e != nil {
		var one uploadItem
		if e2 := json.Unmarshal([]byte(trimmed), &one); e2 == nil {
			items = []uploadItem{one}
		} else if m := fileCodeRe.FindStringSubmatch(trimmed); m != nil {
			items = []uploadItem{{FileCode: m[1], FileStatus: "OK"}}
		} else {
			return "", "", fmt.Errorf("cannot parse the upload response: %s", snippet(body))
		}
	}
	if len(items) == 0 {
		return "", "", fmt.Errorf("empty upload response: %s", snippet(body))
	}

	it := items[0]
	status := strings.ToUpper(strings.TrimSpace(it.FileStatus))
	if it.FileCode == "" || (status != "OK" && status != "") {
		reason := it.FileStatus
		if reason == "" {
			reason = snippet(body)
		}
		return "", "", fmt.Errorf("%s", reason)
	}
	return site + "/" + it.FileCode, it.FileCode, nil
}
