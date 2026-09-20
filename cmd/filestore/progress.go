package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// termWidth: the real width asked of the terminal first, then COLUMNS as a
// manual override, and finally 80 as a safe minimum.
func termWidth() int {
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 40 {
			return n
		}
	}
	if n := termCols(); n >= 40 {
		return n
	}
	return 80
}

// rateMeter estimates the instantaneous rate with an exponential average, so
// the figure does not jump around on every sample.
type rateMeter struct {
	lastBytes int64
	lastTime  time.Time
	rate      float64
}

func (m *rateMeter) update(total int64, now time.Time) float64 {
	if m.lastTime.IsZero() {
		m.lastBytes, m.lastTime = total, now
		return 0
	}
	dt := now.Sub(m.lastTime).Seconds()
	if dt < 0.05 {
		return m.rate
	}
	inst := float64(total-m.lastBytes) / dt
	if inst < 0 {
		inst = 0
	}
	const alpha = 0.3
	if m.rate == 0 {
		m.rate = inst
	} else {
		m.rate = alpha*inst + (1-alpha)*m.rate
	}
	m.lastBytes, m.lastTime = total, now
	return m.rate
}

// Renderer draws the progress. It uses ANSI when attached to a terminal and
// degrades to plain lines otherwise (useful for logs and redirection).
type Renderer struct {
	out       io.Writer
	jobs      []*Job
	dest      string
	workers   int
	totalSize int64
	start     time.Time

	tty       bool
	nameW     int
	barW      int
	mu        sync.Mutex
	lastLines int
	meters    map[*Job]*rateMeter
	totMeter  rateMeter
	announced map[*Job]bool

	stop chan struct{}
	done chan struct{}

	// Plain output prints only on completion, which leaves a log silent for
	// minutes on large files. A periodic line says the transfer is alive.
	plainEvery time.Duration
	lastBeat   time.Time
}

// SetPlainInterval controls how often plain output reports progress.
func (r *Renderer) SetPlainInterval(d time.Duration) { r.plainEvery = d }

func NewRenderer(jobs []*Job, dest string, workers int) *Renderer {
	var total int64
	for _, j := range jobs {
		total += j.Size
	}
	w := termWidth()
	// Reserved: bar + percentage + sizes + rate + padding.
	nameW := w - 58
	if nameW > 52 {
		nameW = 52
	}
	// Never wider than the longest name actually being shown.
	longest := len("TOTAL")
	for _, j := range jobs {
		if n := len([]rune(j.Name)); n > longest {
			longest = n
		}
	}
	if nameW > longest {
		nameW = longest
	}
	if nameW < 14 {
		nameW = 14
	}
	barW := w - nameW - 46
	if barW < 10 {
		barW = 10
	}
	if barW > 24 {
		barW = 24
	}
	return &Renderer{
		out:        os.Stdout,
		nameW:      nameW,
		barW:       barW,
		jobs:       jobs,
		dest:       dest,
		workers:    workers,
		totalSize:  total,
		start:      time.Now(),
		tty:        isTerminal(os.Stdout),
		meters:     make(map[*Job]*rateMeter),
		announced:  make(map[*Job]bool),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		plainEvery: 60 * time.Second,
	}
}

func isTerminal(f *os.File) bool {
	if os.Getenv("FILESTORE_PLAIN") != "" {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func (r *Renderer) Start() {
	go func() {
		defer close(r.done)
		t := time.NewTicker(150 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.render(false)
			}
		}
	}()
}

func (r *Renderer) Stop() {
	close(r.stop)
	<-r.done
	r.render(true)
}

func (r *Renderer) render(final bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	var transferred int64
	var running, done, failed int
	for _, j := range r.jobs {
		transferred += j.Transferred()
		switch j.State() {
		case StateRunning:
			running++
		case StateDone:
			done++
		case StateFailed:
			failed++
		}
	}
	totalRate := r.totMeter.update(transferred, now)

	if !r.tty {
		r.renderPlain(final, done, failed, transferred, totalRate, now)
		return
	}

	var b strings.Builder
	if r.lastLines > 0 {
		fmt.Fprintf(&b, "\033[%dA", r.lastLines)
	}
	// Every line is cut to the terminal width. A line that wraps would occupy
	// two physical rows, the cursor-up count would drift, and the display would
	// smear copies of itself down the screen.
	width := termWidth()
	lines := 0
	writeLine := func(format string, args ...any) {
		fmt.Fprintf(&b, "\033[2K%s\033[0m\n", fitVisible(fmt.Sprintf(format, args...), width-1))
		lines++
	}

	dest := r.dest
	if dest == "" {
		dest = "root"
	}
	writeLine("  %s", bold(truncate(fmt.Sprintf("FileStore · %d files · %s · %d in parallel · → %s",
		len(r.jobs), humanBytes(r.totalSize), r.workers, dest), termWidth()-4)))
	writeLine("")

	shown := 0
	for _, j := range r.jobs {
		st := j.State()
		if st == StateRunning || (final && st == StateFailed) {
			m, ok := r.meters[j]
			if !ok {
				m = &rateMeter{}
				r.meters[j] = m
			}
			rate := m.update(j.Transferred(), now)
			if st == StateFailed {
				_, _, err := j.Result()
				writeLine("  %-*s %s %s", r.nameW, truncateMiddle(j.Name, r.nameW), redX(),
					truncate(errText(err), r.barW+28))
			} else {
				// A retry restarts from zero byte: say so, or the bar looks broken.
				suffix := ""
				if n, msg := j.RetryInfo(); n > 0 {
					suffix = fmt.Sprintf("  \033[33mretry %d: %s\033[0m", n, truncate(msg, 30))
				}
				writeLine("  %-*s %s %3.0f%%  %8s / %-8s %9s%s",
					r.nameW, truncateMiddle(j.Name, r.nameW),
					bar(j.Transferred(), j.Size, r.barW),
					pct(j.Transferred(), j.Size),
					humanBytes(j.Transferred()), humanBytes(j.Size), humanRate(rate), suffix)
			}
			shown++
		}
	}
	if shown == 0 && !final {
		writeLine("  waiting for an upload slot...")
	}

	writeLine("")
	eta := ""
	if totalRate > 1 && transferred < r.totalSize && !final {
		remaining := time.Duration(float64(r.totalSize-transferred)/totalRate) * time.Second
		eta = " · ETA " + shortDur(remaining)
	}
	writeLine("  %s %s %3.0f%%  %8s / %-8s %9s",
		bold(fmt.Sprintf("%-*s", r.nameW, "TOTAL")),
		bar(transferred, r.totalSize, r.barW), pct(transferred, r.totalSize),
		humanBytes(transferred), humanBytes(r.totalSize), humanRate(totalRate))
	writeLine("  %-*s done %d/%d · failed %d · elapsed %s%s",
		r.nameW, "", done, len(r.jobs), failed, shortDur(time.Since(r.start)), eta)

	// When a frame is shorter than the previous one, the leftover lines stay on
	// screen: clear them, then step back up to the end of the new frame.
	if extra := r.lastLines - lines; extra > 0 {
		for i := 0; i < extra; i++ {
			fmt.Fprint(&b, "\033[2K\n")
		}
		fmt.Fprintf(&b, "\033[%dA", extra)
	}

	r.lastLines = lines
	io.WriteString(r.out, b.String())
}

// renderPlain: one line per event, no control codes.
func (r *Renderer) renderPlain(final bool, done, failed int, transferred int64, rate float64, now time.Time) {
	for _, j := range r.jobs {
		st := j.State()
		if (st == StateDone || st == StateFailed) && !r.announced[j] {
			r.announced[j] = true
			link, _, err := j.Result()
			if err != nil {
				fmt.Fprintf(r.out, "FAILED   %s: %v\n", j.Name, err)
			} else {
				fmt.Fprintf(r.out, "OK       %s  %s  (%s in %s)\n",
					j.Name, link, humanBytes(j.Size), shortDur(j.Elapsed()))
			}
		}
	}
	if final {
		fmt.Fprintf(r.out, "Total: %d done, %d failed, %s in %s\n",
			done, failed, humanBytes(r.totalSize), shortDur(time.Since(r.start)))
		return
	}

	// Heartbeat: without it a multi-gigabyte file leaves the log silent for
	// minutes and there is no way to tell progress from a stall.
	if r.plainEvery <= 0 {
		return
	}
	if r.lastBeat.IsZero() {
		r.lastBeat = now
		return
	}
	if now.Sub(r.lastBeat) < r.plainEvery {
		return
	}
	r.lastBeat = now

	var inFlight []string
	for _, j := range r.jobs {
		if j.State() != StateRunning {
			continue
		}
		inFlight = append(inFlight, fmt.Sprintf("%s %.0f%%",
			truncateMiddle(j.Name, 34), pct(j.Transferred(), j.Size)))
	}
	eta := ""
	if rate > 1 && transferred < r.totalSize {
		eta = " · ETA " + shortDur(time.Duration(float64(r.totalSize-transferred)/rate)*time.Second)
	}
	fmt.Fprintf(r.out, "%s  %d/%d done · %s of %s · %s%s\n",
		now.Format("15:04:05"), done, len(r.jobs),
		humanBytes(transferred), humanBytes(r.totalSize), humanRate(rate), eta)
	for _, line := range inFlight {
		fmt.Fprintf(r.out, "           %s\n", line)
	}
}

// --- formattazione ---

func bar(cur, total int64, width int) string {
	filled := 0
	if total > 0 {
		filled = int(float64(cur) / float64(total) * float64(width))
	}
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return "\033[36m" + strings.Repeat("█", filled) + "\033[90m" +
		strings.Repeat("░", width-filled) + "\033[0m"
}

func pct(cur, total int64) float64 {
	if total <= 0 {
		return 0
	}
	p := float64(cur) / float64(total) * 100
	if p > 100 {
		return 100
	}
	return p
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), [...]string{"KB", "MB", "GB", "TB", "PB"}[exp])
}

func humanRate(bps float64) string {
	if bps <= 0 {
		return "--"
	}
	return humanBytes(int64(bps)) + "/s"
}

func shortDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return string(runes[:n])
	}
	return string(runes[:n-3]) + "..."
}

// fitVisible truncates to a number of visible columns, copying ANSI escape
// sequences through without counting them: they move no cursor.
func fitVisible(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	var out strings.Builder
	visible := 0
	for i := 0; i < len(runes); i++ {
		if runes[i] == 0x1b {
			out.WriteRune(runes[i])
			j := i + 1
			if j < len(runes) && runes[j] == '[' {
				out.WriteRune(runes[j])
				j++
			}
			for ; j < len(runes); j++ {
				out.WriteRune(runes[j])
				if (runes[j] >= 'a' && runes[j] <= 'z') || (runes[j] >= 'A' && runes[j] <= 'Z') {
					break
				}
			}
			i = j
			continue
		}
		if visible >= max {
			break
		}
		out.WriteRune(runes[i])
		visible++
	}
	return out.String()
}

// truncateMiddle cuts in the middle rather than at the end: in numbered series
// every file shares the prefix and the distinguishing part sits at the tail.
func truncateMiddle(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 5 {
		return string(runes[:n])
	}
	keepRight := (n - 1) / 2
	keepLeft := n - 1 - keepRight
	return string(runes[:keepLeft]) + "…" + string(runes[len(runes)-keepRight:])
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return truncate(err.Error(), 44)
}

func bold(s string) string { return "\033[1m" + s + "\033[0m" }
func redX() string         { return "\033[31m✗\033[0m" }
