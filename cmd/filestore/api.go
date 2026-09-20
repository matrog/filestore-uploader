package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to FileStore's XFileSharing Pro API.
type Client struct {
	cfg *Config
	hc  *http.Client
}

func NewClient(cfg *Config) *Client {
	// The default transport writes the request body through a 4 KiB buffer,
	// which at upload speed means thousands of syscalls per second: on a large
	// transfer that kernel time dominates the process. A large write buffer
	// cuts it by two orders of magnitude.
	//
	// HTTP/2 is disabled on purpose: upload.cgi is a CGI script, h2 framing
	// only adds per-chunk work and flow-control round trips.
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		WriteBufferSize:       512 << 10,
		ReadBufferSize:        64 << 10,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &Client{
		cfg: cfg,
		// No global timeout: long uploads carry their own context.
		hc: &http.Client{Transport: tr},
	}
}

// flexString accepts both "12" and 12: the API alternates strings and numbers
// on the same field depending on the endpoint, which the blueprint does not say.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(b)
	return nil
}

func (f flexString) String() string { return string(f) }

// envelope is the wrapper shared by every API response.
type envelope struct {
	Status  json.RawMessage `json:"status"`
	Msg     string          `json:"msg"`
	Result  json.RawMessage `json:"result"`
	SessID  string          `json:"sess_id"`
	Session string          `json:"sess"`
}

// statusCode normalises status, which arrives as a number or a string.
func (e envelope) statusCode() int {
	s := strings.Trim(string(e.Status), `"`)
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func (c *Client) apiGet(endpoint string, params url.Values) (*envelope, error) {
	if params == nil {
		params = url.Values{}
	}
	params.Set("key", c.cfg.Key)
	u := fmt.Sprintf("%s/api/%s?%s", c.cfg.Site, endpoint, params.Encode())

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("non-JSON response from /%s: %s", endpoint, snippet(body))
	}
	if code := env.statusCode(); code != 200 {
		msg := env.Msg
		if msg == "" {
			msg = snippet(body)
		}
		return nil, fmt.Errorf("API /%s: %s (status %d)", endpoint, msg, code)
	}
	return &env, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// --- Account ---

type AccountInfo struct {
	Email         string     `json:"email"`
	Balance       flexString `json:"balance"`
	StorageUsed   flexString `json:"storage_used"`
	StorageLeft   flexString `json:"storage_left"`
	Premium       flexString `json:"premium"`
	PremiumExpire flexString `json:"premium_expire"`
}

// Tier maps the account onto the value upload.cgi expects: "prem" only when
// the account really is premium, "reg" otherwise. Claiming a tier the account
// does not have is neither honest nor useful: the server checks the session.
func (a *AccountInfo) Tier() string {
	switch strings.ToLower(strings.TrimSpace(a.Premium.String())) {
	case "1", "true", "yes", "prem", "premium":
		return "prem"
	}
	// Some installations leave `premium` at 0 and only record an expiry date.
	if exp := strings.TrimSpace(a.PremiumExpire.String()); exp != "" && exp != "0" {
		for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
			if t, err := time.Parse(layout, exp); err == nil {
				if t.After(time.Now()) {
					return "prem"
				}
				break
			}
		}
	}
	return "reg"
}

// PremiumUntil parses the expiry date, when there is a usable one.
func (a *AccountInfo) PremiumUntil() (time.Time, bool) {
	exp := strings.TrimSpace(a.PremiumExpire.String())
	if exp == "" || exp == "0" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, exp); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Describe renders the account in the terms a person cares about: which tier,
// and until when.
func (a *AccountInfo) Describe() string {
	tier := a.Tier()
	label := "Registered"
	if tier == "prem" {
		label = "Premium"
	}
	until, ok := a.PremiumUntil()
	if !ok {
		if tier == "prem" {
			return label
		}
		return label + " (no premium)"
	}

	days := int(time.Until(until).Hours() / 24)
	when := until.Format("2 January 2006")
	switch {
	case days < 0:
		return fmt.Sprintf("Registered (premium expired on %s)", when)
	case days == 0:
		return fmt.Sprintf("%s (expires today, %s)", label, until.Format("15:04"))
	case days == 1:
		return fmt.Sprintf("%s (expires tomorrow, %s)", label, when)
	default:
		return fmt.Sprintf("%s (expires %s, %d days left)", label, when, days)
	}
}

func (c *Client) AccountInfo() (*AccountInfo, error) {
	env, err := c.apiGet("account/info", nil)
	if err != nil {
		return nil, err
	}
	var a AccountInfo
	if err := json.Unmarshal(env.Result, &a); err != nil {
		return nil, fmt.Errorf("cannot read account/info: %w", err)
	}
	return &a, nil
}

// --- Upload server ---

// UploadTarget is the destination of a single upload. The sess_id is single
// use: it must be requested for every file.
type UploadTarget struct {
	URL    string
	SessID string
}

func (c *Client) UploadServer() (*UploadTarget, error) {
	env, err := c.apiGet("upload/server", nil)
	if err != nil {
		return nil, err
	}
	var raw string
	if err := json.Unmarshal(env.Result, &raw); err != nil {
		// Some installations nest the URL inside an object.
		var obj struct {
			UploadURL string `json:"upload_url"`
			URL       string `json:"url"`
			Server    string `json:"server"`
			SessID    string `json:"sess_id"`
		}
		if err2 := json.Unmarshal(env.Result, &obj); err2 != nil {
			return nil, fmt.Errorf("no upload endpoint in the response")
		}
		for _, cand := range []string{obj.UploadURL, obj.URL, obj.Server} {
			if strings.HasPrefix(cand, "http") {
				raw = cand
				break
			}
		}
		if obj.SessID != "" {
			env.SessID = obj.SessID
		}
	}
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "http") {
		return nil, fmt.Errorf("invalid upload URL: %q", raw)
	}

	sess := env.SessID
	if sess == "" {
		sess = env.Session
	}
	// Legacy variant: sess_id inside the URL query string.
	if sess == "" {
		if parsed, err := url.Parse(raw); err == nil {
			q := parsed.Query()
			if v := q.Get("sess_id"); v != "" {
				sess = v
			} else if v := q.Get("sess"); v != "" {
				sess = v
			}
		}
	}
	// Last resort, as the public blueprint implies: the API key acts as session.
	if sess == "" {
		sess = c.cfg.Key
	}
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	return &UploadTarget{URL: raw, SessID: sess}, nil
}

// --- Cartelle ---

type Folder struct {
	Name  string     `json:"name"`
	FldID flexString `json:"fld_id"`
}

type RemoteFile struct {
	Name     string     `json:"name"`
	FileCode string     `json:"file_code"`
	FldID    flexString `json:"fld_id"`
	Uploaded string     `json:"uploaded"`
}

// FolderList lists the subfolders and files of a folder (empty = root).
func (c *Client) FolderList(fldID string) ([]Folder, []RemoteFile, error) {
	params := url.Values{}
	if fldID != "" && fldID != "0" {
		params.Set("fld_id", fldID)
	}
	env, err := c.apiGet("folder/list", params)
	if err != nil {
		return nil, nil, err
	}
	var r struct {
		Folders []Folder     `json:"folders"`
		Files   []RemoteFile `json:"files"`
	}
	if err := json.Unmarshal(env.Result, &r); err != nil {
		return nil, nil, fmt.Errorf("cannot read folder/list: %w", err)
	}
	return r.Folders, r.Files, nil
}

// SetFolder moves one or more files into a folder in a single call.
func (c *Client) SetFolder(codes []string, fldID string) error {
	if len(codes) == 0 || fldID == "" {
		return nil
	}
	params := url.Values{}
	params.Set("file_code", strings.Join(codes, ","))
	params.Set("fld_id", fldID)
	_, err := c.apiGet("file/set_folder", params)
	return err
}

// CreateFolder creates a folder and returns its id when the API provides one.
func (c *Client) CreateFolder(name, parentID string) (string, error) {
	params := url.Values{}
	params.Set("name", name)
	if parentID != "" && parentID != "0" {
		params.Set("parent_id", parentID)
	}
	env, err := c.apiGet("folder/create", params)
	if err != nil {
		return "", err
	}
	var r struct {
		FldID any `json:"fld_id"`
	}
	_ = json.Unmarshal(env.Result, &r)
	switch v := r.FldID.(type) {
	case string:
		return v, nil
	case float64:
		return strconv.Itoa(int(v)), nil
	}
	return "", nil
}

// ListedFile is one entry of /api/file/list. The code arrives under different
// names depending on the endpoint and the installation — "filecode" in the
// blueprint, "file_code" elsewhere — so every spelling is accepted. Reading
// only one of them yields an empty code and silently discards the whole
// listing, which is exactly what made the pre-upload check a no-op.
type ListedFile struct {
	Status   flexString
	FileCode string
	Name     string
	Size     flexString
	Uploaded string
	Link     string
}

func (f *ListedFile) UnmarshalJSON(b []byte) error {
	var r struct {
		Status      flexString `json:"status"`
		FileCode    string     `json:"filecode"`
		FileCodeAlt string     `json:"file_code"`
		CodeAlt     string     `json:"code"`
		Name        string     `json:"name"`
		FileName    string     `json:"file_name"`
		Size        flexString `json:"size"`
		FileSize    flexString `json:"file_size"`
		Uploaded    string     `json:"uploaded"`
		Link        string     `json:"link"`
		DownloadURL string     `json:"download_url"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	f.Status = r.Status
	f.Name = firstNonEmpty(r.Name, r.FileName)
	f.Uploaded = r.Uploaded
	f.Link = firstNonEmpty(r.Link, r.DownloadURL)
	f.Size = r.Size
	if strings.TrimSpace(f.Size.String()) == "" {
		f.Size = r.FileSize
	}
	f.FileCode = firstNonEmpty(r.FileCode, r.FileCodeAlt, r.CodeAlt)
	if f.FileCode == "" {
		f.FileCode = codeFromLink(f.Link)
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// codeFromLink pulls the code out of a download URL such as
// https://filestore.me/4w0sy8e63f0c.html — the last resort when no field holds it.
func codeFromLink(link string) string {
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	seg := strings.Trim(u.Path, "/")
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	seg = strings.TrimSuffix(seg, ".html")
	if len(seg) < 6 || len(seg) > 30 {
		return ""
	}
	for _, r := range seg {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return ""
		}
	}
	return seg
}

// FindRecent looks for recently uploaded files matching a name. It answers the
// question "did this upload actually land?" after a connection failed, which is
// what makes retrying a multi-gigabyte file avoidable.
func (c *Client) FindRecent(name string, minutes int) ([]ListedFile, error) {
	params := url.Values{}
	params.Set("name", name)
	params.Set("created", strconv.Itoa(minutes))
	params.Set("per_page", "100")

	env, err := c.apiGet("file/list", params)
	if err != nil {
		return nil, err
	}
	var files []ListedFile
	if err := json.Unmarshal(env.Result, &files); err != nil {
		// Some installations wrap the list in an object.
		var wrapped struct {
			Files []ListedFile `json:"files"`
		}
		if err2 := json.Unmarshal(env.Result, &wrapped); err2 != nil {
			return nil, fmt.Errorf("cannot read file/list: %w", err)
		}
		files = wrapped.Files
	}
	return files, nil
}

// FolderFiles enumerates a folder's files with their sizes, paging until the
// server runs out. file/list is used rather than folder/list because only the
// former reports a size, which is what makes a safe skip decision possible.
func (c *Client) FolderFiles(fldID string, maxPages int) ([]ListedFile, error) {
	const perPage = 100
	var all []ListedFile

	for page := 1; page <= maxPages; page++ {
		params := url.Values{}
		if fldID != "" && fldID != "0" {
			params.Set("fld_id", fldID)
		}
		params.Set("per_page", strconv.Itoa(perPage))
		params.Set("page", strconv.Itoa(page))

		env, err := c.apiGet("file/list", params)
		if err != nil {
			return all, err
		}
		var batch []ListedFile
		if err := json.Unmarshal(env.Result, &batch); err != nil {
			var wrapped struct {
				Files []ListedFile `json:"files"`
			}
			if err2 := json.Unmarshal(env.Result, &wrapped); err2 != nil {
				return all, fmt.Errorf("cannot read file/list: %w", err)
			}
			batch = wrapped.Files
		}
		all = append(all, batch...)
		if len(batch) < perPage {
			break
		}
	}
	return all, nil
}
