package main

import (
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// cmdFiles lists the files in a remote folder, which is how a finished
// transfer gets verified: a split archive needs every one of its parts.
func cmdFiles(args []string) error {
	fs := flag.NewFlagSet("files", flag.ExitOnError)
	id := fs.String("id", "", "folder id (empty = root)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	files, err := NewClient(cfg).FolderFiles(*id, 60)
	if err != nil {
		return err
	}

	where := "root"
	if *id != "" {
		where = "folder " + *id
	}
	if len(files) == 0 {
		fmt.Printf("No files in %s.\n", where)
		return nil
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	fmt.Printf("Files in %s:\n\n", where)
	fmt.Printf("  %-48s %10s  %-14s %s\n", "NAME", "SIZE", "CODE", "UPLOADED")
	var total int64
	for _, f := range files {
		size := "?"
		if n, cerr := strconv.ParseInt(strings.TrimSpace(f.Size.String()), 10, 64); cerr == nil {
			size = humanBytes(n)
			total += n
		}
		fmt.Printf("  %-48s %10s  %-14s %s\n",
			truncateMiddle(f.Name, 48), size, f.FileCode, f.Uploaded)
	}
	fmt.Printf("\n  %d files, %s\n", len(files), humanBytes(total))
	return nil
}
