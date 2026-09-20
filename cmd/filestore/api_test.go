package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The real server alternates strings and numbers on the same fields:
// folder/list returns fld_id as a number, the blueprint documents a string.
func TestFlexStringAcceptsBothForms(t *testing.T) {
	var out struct {
		Folders []Folder     `json:"folders"`
		Files   []RemoteFile `json:"files"`
	}
	body := `{
		"folders":[{"name":"Documenti","fld_id":101},{"name":"Video","fld_id":"102"}],
		"files":[{"name":"a.zip","file_code":"abc123","fld_id":101}]
	}`
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if got := out.Folders[0].FldID.String(); got != "101" {
		t.Errorf("numeric fld_id = %q, expected \"101\"", got)
	}
	if got := out.Folders[1].FldID.String(); got != "102" {
		t.Errorf("string fld_id = %q, expected \"102\"", got)
	}
	if got := out.Files[0].FldID.String(); got != "101" {
		t.Errorf("file fld_id = %q, expected \"101\"", got)
	}
}

func TestAccountInfoWithNumericFields(t *testing.T) {
	var a AccountInfo
	body := `{"email":"x@y.it","storage_used":24186265,"storage_left":"10737418240","premium":1,"balance":0}`
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if a.StorageUsed.String() != "24186265" || a.StorageLeft.String() != "10737418240" {
		t.Errorf("storage parsed wrong: used=%q left=%q", a.StorageUsed, a.StorageLeft)
	}
	if a.Premium.String() != "1" {
		t.Errorf("premium = %q, expected \"1\"", a.Premium)
	}
}

func TestStatusCodeStringOrNumber(t *testing.T) {
	for _, raw := range []string{`{"status":200}`, `{"status":"200"}`} {
		var e envelope
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if e.statusCode() != 200 {
			t.Errorf("%s -> statusCode %d, expected 200", raw, e.statusCode())
		}
	}
}

// The tier decides which size ceiling the upload server applies. It must follow
// the account, never assume the most permissive value.
func TestAccountTier(t *testing.T) {
	future := time.Now().Add(48 * time.Hour).Format("2006-01-02 15:04:05")
	past := time.Now().Add(-48 * time.Hour).Format("2006-01-02 15:04:05")

	cases := []struct {
		name    string
		premium string
		expire  string
		want    string
	}{
		{"premium flag set", "1", "", "prem"},
		{"premium as word", "premium", "", "prem"},
		{"plain registered", "0", "", "reg"},
		{"empty account", "", "", "reg"},
		{"expiry in the future", "0", future, "prem"},
		{"expiry already passed", "0", past, "reg"},
		{"nonsense expiry", "0", "never", "reg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &AccountInfo{Premium: flexString(tc.premium), PremiumExpire: flexString(tc.expire)}
			if got := a.Tier(); got != tc.want {
				t.Errorf("Tier() = %q, expected %q", got, tc.want)
			}
		})
	}
}

func TestAccountDescribe(t *testing.T) {
	in30 := time.Now().Add(30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	gone := time.Now().Add(-5 * 24 * time.Hour).Format("2006-01-02 15:04:05")

	cases := []struct {
		name    string
		info    AccountInfo
		wantSub string
	}{
		{"premium with expiry", AccountInfo{Premium: "", PremiumExpire: flexString(in30)}, "Premium (expires"},
		{"premium without expiry", AccountInfo{Premium: "1"}, "Premium"},
		{"expired premium", AccountInfo{Premium: "0", PremiumExpire: flexString(gone)}, "premium expired on"},
		{"plain account", AccountInfo{Premium: "0"}, "Registered (no premium)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.Describe(); !strings.Contains(got, tc.wantSub) {
				t.Errorf("Describe() = %q, expected it to contain %q", got, tc.wantSub)
			}
		})
	}
}

// The real server does not use the field name the blueprint documents. Reading
// only one spelling produced an empty code, which silently discarded every
// listed file and turned the pre-upload skip check into a no-op.
func TestListedFileCodeVariants(t *testing.T) {
	cases := map[string]string{
		`{"filecode":"aaa111bbb222","name":"a.z01","size":"100"}`:      "aaa111bbb222",
		`{"file_code":"ccc333ddd444","name":"b.z02","size":"200"}`:     "ccc333ddd444",
		`{"code":"eee555fff666","name":"c.z03"}`:                       "eee555fff666",
		`{"link":"https://filestore.me/ggg777hhh888.html","name":"d"}`: "ggg777hhh888",
		`{"name":"no code here"}`:                                      "",
	}
	for body, want := range cases {
		var f ListedFile
		if err := json.Unmarshal([]byte(body), &f); err != nil {
			t.Fatalf("unmarshal %s: %v", body, err)
		}
		if f.FileCode != want {
			t.Errorf("%s -> code %q, expected %q", body, f.FileCode, want)
		}
	}
}

func TestListedFileNameAndSizeVariants(t *testing.T) {
	var f ListedFile
	if err := json.Unmarshal([]byte(`{"file_code":"x1y2z3a4b5c6","file_name":"part.z01","file_size":4718592000}`), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Name != "part.z01" {
		t.Errorf("name = %q", f.Name)
	}
	if f.Size.String() != "4718592000" {
		t.Errorf("size = %q", f.Size.String())
	}
}
