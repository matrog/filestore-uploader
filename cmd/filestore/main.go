package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version is stamped at build time with -ldflags "-X main.version=...".
// "dev" is what a plain `go build` produces.
var version = "dev"

func main() {
	enableVT()

	if len(os.Args) < 2 {
		usage()
		os.Exit(0)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "setup":
		err = cmdSetup()
	case "check":
		err = cmdCheck()
	case "folders", "ls":
		err = cmdFolders(args)
	case "up", "upload":
		err = cmdUp(args)
	case "files":
		err = cmdFiles(args)
	case "version", "-v", "--version":
		fmt.Println("FileStore Uploader " + version)
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command: %s (try \"filestore help\")", cmd)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`FileStore Uploader ` + version + ` — upload files to FileStore.me

COMMANDS
  setup                  store the API key in ~/.filestore.conf
  check                  verify the key, quota and account status
  folders                list the remote folder tree with ids
  up [options] [paths]   upload files and folders
  files [-id FLD]        list the files in a remote folder

OPTIONS FOR "up"
  -from DIR       source folder (alternative to positional paths)
  -to NAME|ID     destination folder (also "Parent/Child")
  -n N            parallel uploads (default 3)
  -r              descend into subfolders
  -include GLOB   filter by name, e.g. -include '*.mp4'
  -links FILE     append the resulting links to a file
  -create         create the destination folder when missing
  -plain          plain output, one line per file (logs, scripts)
  -progress-every D  how often plain output reports progress (default 1m)
  -state FILE     resume file (default .filestore-state.jsonl)
  -no-resume      ignore the state file and upload everything again
  -check-remote   skip files already in the destination with the same size (default on)
  -retries N      retries per file after a network failure (default 2, 0 off)
  -utype TIER     account tier for the upload server: prem or reg (auto by default)

RESUMING
  Every confirmed file is recorded immediately. If a transfer is interrupted,
  run the very same command again: it picks up where it left off.

EXAMPLES
  filestore up -from ~/Videos -to Movies -n 3 -include '*.mkv'
  filestore up -to 12345 archive.zip notes.pdf
  filestore folders
  caffeinate -i filestore up -from /Volumes/Data -to rainbow -state ~/state.jsonl
`)
}

func cmdSetup() error {
	fmt.Printf("Get your API key from the account panel: %s/?op=my_account\n", siteURL())
	fmt.Print("Paste the API key: ")
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return errors.New("no key entered")
	}
	key := strings.TrimSpace(sc.Text())
	if key == "" {
		return errors.New("no key entered")
	}
	if err := saveKey(key); err != nil {
		return err
	}
	fmt.Printf("Saved to %s (mode 600)\n\n", configPath())
	return cmdCheck()
}

func cmdCheck() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	info, err := NewClient(cfg).AccountInfo()
	if err != nil {
		return err
	}
	fmt.Println("Account OK")
	fmt.Printf("  %-22s %s\n", "email:", info.Email)
	fmt.Printf("  %-22s %s\n", "tier:", info.Describe())
	fmt.Printf("  %-22s %s\n", "sent as utype:", info.Tier())
	fmt.Printf("  %-22s %s\n", "storage used:", humanBytes(parseBytes(info.StorageUsed.String())))
	fmt.Printf("  %-22s %s\n", "storage left:", humanBytes(parseBytes(info.StorageLeft.String())))
	return nil
}

func parseBytes(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func cmdFolders(args []string) error {
	fset := flag.NewFlagSet("folders", flag.ExitOnError)
	id := fset.String("id", "", "start from this folder instead of the root")
	depth := fset.Int("depth", 3, "how many levels to show")
	if err := fset.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	c := NewClient(cfg)

	where := "root"
	if *id != "" {
		where = "folder " + *id
	}
	fmt.Printf("FileStore folders (from %s):\n\n", where)

	total, err := walkFolders(c, *id, "", 0, *depth)
	if err != nil {
		return err
	}
	if total == 0 {
		fmt.Println("  (no subfolders)")
	}
	fmt.Printf("\n  %d folders found\n", total)
	fmt.Println("\nExamples:")
	fmt.Println("  filestore up -to \"Name\" file...          top-level folder")
	fmt.Println("  filestore up -to \"Parent/Child\" file...  nested folder")
	fmt.Println("  filestore up -to 12345 file...           by id, always unambiguous")
	return nil
}

// walkFolders prints the folder tree, one level per API call.
func walkFolders(c *Client, id, prefix string, level, maxDepth int) (int, error) {
	if level >= maxDepth {
		return 0, nil
	}
	folders, files, err := c.FolderList(id)
	if err != nil {
		return 0, err
	}
	if level == 0 && len(files) > 0 {
		fmt.Printf("  %-36s %d files\n", "(root)", len(files))
	}
	count := 0
	for _, f := range folders {
		count++
		label := prefix + f.Name
		_, sub, err := c.FolderList(f.FldID.String())
		nFiles := 0
		if err == nil {
			nFiles = len(sub)
		}
		fmt.Printf("  %-36s id %-8s %d files\n",
			strings.Repeat("  ", level)+label+"/", f.FldID.String(), nFiles)
		n, err := walkFolders(c, f.FldID.String(), label+"/", level+1, maxDepth)
		if err != nil {
			return count, err
		}
		count += n
	}
	return count, nil
}

func cmdUp(args []string) error {
	fset := flag.NewFlagSet("up", flag.ExitOnError)
	from := fset.String("from", "", "source folder")
	to := fset.String("to", "", "destination folder (name or id)")
	workers := fset.Int("n", 3, "parallel uploads")
	recursive := fset.Bool("r", false, "descend into subfolders")
	include := fset.String("include", "", "glob filter on the file name")
	linksFile := fset.String("links", "", "append the links to this file")
	create := fset.Bool("create", false, "create the destination folder when missing")
	plain := fset.Bool("plain", false, "plain output, no progress bars")
	state := fset.String("state", ".filestore-state.jsonl", "state file used to resume an interrupted transfer")
	noResume := fset.Bool("no-resume", false, "ignore the state file and upload everything again")
	retries := fset.Int("retries", 2, "retries per file after a network failure (0 disables them)")
	utype := fset.String("utype", "", "account tier sent to the upload server: prem, reg (default: from the account)")
	progressEvery := fset.Duration("progress-every", 60*time.Second, "how often plain output reports progress (0 disables it)")
	checkRemote := fset.Bool("check-remote", true, "before uploading, skip files already in the destination with the same size")
	// The standard parser stops at the first positional argument; here options
	// and paths may be mixed freely.
	var positional []string
	rest := args
	for {
		if err := fset.Parse(rest); err != nil {
			return err
		}
		if fset.NArg() == 0 {
			break
		}
		positional = append(positional, fset.Arg(0))
		rest = fset.Args()[1:]
	}
	if *plain {
		os.Setenv("FILESTORE_PLAIN", "1")
	}
	if *workers < 1 {
		return errors.New("-n must be at least 1")
	}

	sources := positional
	if *from != "" {
		sources = append(sources, *from)
	}
	if len(sources) == 0 {
		return errors.New("no source: pass some paths or use -from DIR")
	}

	paths, err := collectFiles(sources, *recursive, *include)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return errors.New("no file matches the given criteria")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	client := NewClient(cfg)

	destID, destName := "", ""
	if *to != "" {
		destID, destName, err = resolveFolder(client, *to, *create)
		if err != nil {
			return err
		}
	}

	var store *StateStore
	if *state != "" {
		store, err = OpenStateStore(*state)
		if err != nil {
			return err
		}
		defer store.Close()
	}

	links, err := OpenLinkLog(*linksFile)
	if err != nil {
		return err
	}
	defer links.Close()

	jobs := make([]*Job, 0, len(paths))
	var skipped []StateEntry
	var skippedBytes int64
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		if store != nil && !*noResume {
			if e, ok := store.Done(p, st.Size()); ok {
				skipped = append(skipped, e)
				skippedBytes += e.Size
				continue
			}
		}
		jobs = append(jobs, &Job{Path: p, Name: filepath.Base(p), Size: st.Size()})
	}

	// A remote scan catches files put there by any other means — the web
	// uploader, an earlier run whose state file was lost — so nothing already
	// on the server is sent twice.
	if *checkRemote && len(jobs) > 0 {
		// The destination is the contract: a file counts as present only if it
		// is in the folder that was asked for. Anything sitting elsewhere —
		// the root, another folder — is not "in test", so it gets uploaded.
		remote, rerr := client.FolderFiles(destID, 60)
		if rerr != nil {
			fmt.Printf("Remote check skipped (%v)\n\n", rerr)
		} else {
			bySize := make(map[string]ListedFile, len(remote))
			nameOnly := make(map[string]bool, len(remote))
			for _, f := range remote {
				if f.FileCode == "" {
					continue
				}
				nameOnly[strings.ToLower(f.Name)] = true
				if n, convErr := strconv.ParseInt(strings.TrimSpace(f.Size.String()), 10, 64); convErr == nil {
					bySize[fmt.Sprintf("%s|%d", strings.ToLower(f.Name), n)] = f
				}
			}

			kept := jobs[:0]
			var mismatched int
			for _, j := range jobs {
				key := fmt.Sprintf("%s|%d", strings.ToLower(j.Name), j.Size)
				if f, ok := bySize[key]; ok {
					// Found in the destination itself, so nothing to move.
					e := StateEntry{
						Path: j.Path, Size: j.Size, Code: f.FileCode,
						Link: cfg.Site + "/" + f.FileCode,
						When: time.Now().Format(time.RFC3339), FldID: destID,
					}
					skipped = append(skipped, e)
					skippedBytes += j.Size
					if store != nil {
						_ = store.Add(e)
					}
					continue
				}
				// Same name, different size: not safe to assume it is the same
				// file, so it is uploaded and the ambiguity is reported.
				if nameOnly[strings.ToLower(j.Name)] {
					mismatched++
				}
				kept = append(kept, j)
			}
			jobs = kept

			label := "destination folder"
			if destID == "" {
				label = "root folder"
			}
			fmt.Printf("The %s holds %d files; %d of yours are already there.\n",
				label, len(remote), len(skipped))
			if mismatched > 0 {
				fmt.Printf("  %d have a matching name but a different size — uploaded again to be safe.\n", mismatched)
			}
			fmt.Println()
		}
	}

	if len(skipped) > 0 {
		fmt.Printf("Already on the server: %d files (%s), %d to go\n\n",
			len(skipped), humanBytes(skippedBytes), len(jobs))
	}
	if len(jobs) == 0 {
		fmt.Println("Nothing left to upload.")
		return finalizeFolder(client, nil, skipped, destID, destName, store)
	}
	if len(jobs) == 0 {
		fmt.Println("Every file is already uploaded. Use -no-resume to redo them.")
		return finalizeFolder(client, nil, skipped, destID, destName, store)
	}
	if store != nil {
		fmt.Printf("State in %s — if the transfer stops, run the same command again.\n\n", *state)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "\nInterrupt requested, closing the uploads in flight...")
		cancel()
	}()

	up := NewUploader(client, *workers, *retries)

	// The upload server applies size limits per account tier. Ask the account
	// which tier it is instead of guessing: sending nothing means "anonymous",
	// capped at 10 MB.
	tier := *utype
	if tier == "" {
		tier = "reg"
		if info, aerr := client.AccountInfo(); aerr == nil {
			tier = info.Tier()
			fmt.Printf("Account: %s\n", info.Describe())
		} else {
			fmt.Printf("Account: unknown (%v) — using the registered tier\n", aerr)
		}
	} else {
		label := "Registered"
		if tier == "prem" {
			label = "Premium"
		}
		fmt.Printf("Account: %s (forced with -utype %s)\n", label, tier)
	}
	up.SetUserType(tier)

	// Events go to a file, never to the terminal: writing to stderr while the
	// progress block redraws corrupts the display and the message is lost.
	logPath := *state + ".log"
	if *state == "" {
		logPath = "filestore-events.log"
	}
	evLog, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("event log %s: %w", logPath, err)
	}
	defer evLog.Close()
	var logMu sync.Mutex
	logEvent := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(evLog, "%s  %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
	}
	events := 0

	up.OnSuccess = func(j *Job, link, code string) {
		// Move it into the destination straight away. Deferring every move to
		// the end means an interrupted run leaves files stranded in the root.
		placed := ""
		if destID != "" {
			if serr := client.SetFolder([]string{code}, destID); serr != nil {
				logEvent("MOVE FAILED  %s  could not be placed in %q: %v", j.Name, destName, serr)
			} else {
				placed = destID
				j.MarkPlaced()
			}
		}
		if store != nil {
			_ = store.Add(StateEntry{
				Path: j.Path, Size: j.Size, Code: code, Link: link,
				When: time.Now().Format(time.RFC3339), FldID: placed,
			})
		}
		links.Add(j.Name, link)
	}

	up.OnRetry = func(j *Job, attempt int, err error) {
		logMu.Lock()
		events++
		logMu.Unlock()
		logEvent("RETRY %d/%d  %s  after: %v", attempt, *retries, j.Name, err)
	}
	up.OnLimit = func(limit int64) {
		logEvent("LIMIT  the server refuses files above %s — larger files are now skipped without sending", humanBytes(limit))
	}
	up.OnVerified = func(j *Job, link, code string) {
		logMu.Lock()
		events++
		logMu.Unlock()
		logEvent("VERIFIED  %s  the upload had landed despite the error: %s", j.Name, link)
	}
	fmt.Printf("Events (retries, verifications) logged to %s\n\n", logPath)

	r := NewRenderer(jobs, destName, *workers)
	r.SetPlainInterval(*progressEvery)
	r.Start()
	up.Run(ctx, jobs)
	r.Stop()

	if events > 0 {
		fmt.Printf("\n  %d retry/verification events — details in %s\n", events, logPath)
	}
	return report(client, jobs, skipped, destID, destName, *linksFile, store)
}

func report(c *Client, jobs []*Job, skipped []StateEntry, destID, destName, linksFile string, store *StateStore) error {
	var uploaded, failed, interrupted, notAttempted int
	for _, j := range jobs {
		_, code, err := j.Result()
		switch {
		case code != "":
			uploaded++
		case errors.Is(err, ErrInterrupted):
			interrupted++
		case err != nil:
			failed++
		default:
			// Queued when the run ended: never sent, so never a success.
			notAttempted++
		}
	}

	if err := finalizeFolder(c, jobs, skipped, destID, destName, store); err != nil {
		fmt.Fprintf(os.Stderr, "\nWarning: %v\n", err)
	}

	fmt.Println()
	// Identical failures are grouped: 299 copies of one message teach nothing
	// and bury the successes.
	const maxPerReason = 3
	seenReason := make(map[string]int)
	var shownFailures int
	for _, j := range jobs {
		link, code, err := j.Result()
		switch {
		case code != "":
			fmt.Printf("  \033[32m✓\033[0m %-40s %s\n", truncateMiddle(j.Name, 40), link)
		case errors.Is(err, ErrInterrupted):
			// Only counted in the summary.
		case err != nil:
			reason := err.Error()
			seenReason[reason]++
			if seenReason[reason] <= maxPerReason {
				fmt.Printf("  \033[31m✗\033[0m %-40s %v\n", truncateMiddle(j.Name, 40), err)
				shownFailures++
			}
		}
	}
	for reason, n := range seenReason {
		if n > maxPerReason {
			fmt.Printf("  \033[31m✗\033[0m %-40s %s\n",
				fmt.Sprintf("... and %d more files", n-maxPerReason), truncate(reason, 60))
		}
	}
	_ = shownFailures

	fmt.Println()
	fmt.Printf("  uploaded:      %d\n", uploaded)
	if failed > 0 {
		fmt.Printf("  failed:        %d\n", failed)
	}
	if interrupted > 0 {
		fmt.Printf("  interrupted:   %d\n", interrupted)
	}
	if notAttempted > 0 {
		fmt.Printf("  not attempted: %d\n", notAttempted)
	}
	if len(skipped) > 0 {
		fmt.Printf("  already there: %d (from earlier runs)\n", len(skipped))
	}
	if linksFile != "" && uploaded > 0 {
		fmt.Printf("\n  Links appended to %s\n", linksFile)
	}

	if remaining := failed + interrupted + notAttempted; remaining > 0 {
		return fmt.Errorf("%d of %d files still to upload — run the same command again to resume",
			remaining, len(jobs))
	}
	return nil
}

// finalizeFolder moves into the destination everything not yet there,
// including files uploaded by an earlier session that stopped before the move.
func finalizeFolder(c *Client, jobs []*Job, skipped []StateEntry, destID, destName string, store *StateStore) error {
	if destID == "" {
		return nil
	}
	var codes []string
	var entries []StateEntry

	for _, j := range jobs {
		link, code, err := j.Result()
		if err != nil || code == "" || j.Placed() {
			// Already moved the moment it finished uploading.
			continue
		}
		codes = append(codes, code)
		entries = append(entries, StateEntry{Path: j.Path, Size: j.Size, Code: code, Link: link})
	}
	for _, e := range skipped {
		if e.FldID != destID {
			codes = append(codes, e.Code)
			entries = append(entries, e)
		}
	}
	if len(codes) == 0 {
		return nil
	}

	// set_folder takes several codes per call; batched so the query string
	// cannot grow past its maximum length.
	const batch = 50
	for i := 0; i < len(codes); i += batch {
		end := i + batch
		if end > len(codes) {
			end = len(codes)
		}
		if err := c.SetFolder(codes[i:end], destID); err != nil {
			return fmt.Errorf("files uploaded but not moved into %q: %w", destName, err)
		}
	}
	if store != nil {
		for _, e := range entries {
			e.FldID = destID
			e.When = time.Now().Format(time.RFC3339)
			_ = store.Add(e)
		}
	}
	return nil
}

// collectFiles expands paths and folders into a flat list of files.
func collectFiles(sources []string, recursive bool, include string) ([]string, error) {
	var out []string
	seen := make(map[string]bool)

	add := func(p string) {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}

	match := func(name string) bool {
		if include == "" {
			return true
		}
		ok, err := filepath.Match(include, name)
		return err == nil && ok
	}

	for _, src := range sources {
		st, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src, err)
		}
		if !st.IsDir() {
			if match(filepath.Base(src)) {
				add(src)
			}
			continue
		}
		if recursive {
			err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
					return nil
				}
				if match(d.Name()) {
					add(p)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if match(e.Name()) {
				add(filepath.Join(src, e.Name()))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// resolveFolder accepts a numeric id, a name, or a "Parent/Child" path.
func resolveFolder(c *Client, spec string, create bool) (id, name string, err error) {
	if isNumeric(spec) {
		return spec, spec, nil
	}
	parts := strings.Split(strings.Trim(spec, "/"), "/")
	currentID := ""
	currentName := ""
	for _, part := range parts {
		folders, _, err := c.FolderList(currentID)
		if err != nil {
			return "", "", err
		}
		found := ""
		for _, f := range folders {
			if strings.EqualFold(f.Name, part) {
				found = f.FldID.String()
				break
			}
		}
		if found == "" {
			if !create {
				avail := make([]string, 0, len(folders))
				for _, f := range folders {
					avail = append(avail, fmt.Sprintf("%q", f.Name))
				}
				hint := "no folders available"
				if len(avail) > 0 {
					hint = "available: " + strings.Join(avail, ", ")
				}
				return "", "", fmt.Errorf("folder %q not found (%s). Use -create to create it", part, hint)
			}
			newID, err := c.CreateFolder(part, currentID)
			if err != nil {
				return "", "", fmt.Errorf("creating %q: %w", part, err)
			}
			if newID == "" {
				// The API returned no id: read it back from the listing.
				folders, _, err := c.FolderList(currentID)
				if err != nil {
					return "", "", err
				}
				for _, f := range folders {
					if strings.EqualFold(f.Name, part) {
						newID = f.FldID.String()
						break
					}
				}
			}
			if newID == "" {
				return "", "", fmt.Errorf("folder %q created but its id could not be retrieved", part)
			}
			found = newID
		}
		currentID, currentName = found, part
	}
	return currentID, currentName, nil
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
