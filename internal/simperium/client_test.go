package simperium

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNoteUnmarshalLenient(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Note
	}{
		{
			name: "normal booleans and numbers",
			raw:  `{"content":"hi","deleted":true,"creationDate":1,"modificationDate":2,"tags":["a"],"systemTags":["pinned"]}`,
			want: Note{Content: "hi", Deleted: true, CreationDate: 1, ModificationDate: 2, Tags: []string{"a"}, SystemTags: []string{"pinned"}},
		},
		{
			name: "numeric deleted",
			raw:  `{"content":"hi","deleted":1,"creationDate":1,"modificationDate":2}`,
			want: Note{Content: "hi", Deleted: true, CreationDate: 1, ModificationDate: 2},
		},
		{
			name: "zero deleted",
			raw:  `{"content":"hi","deleted":0}`,
			want: Note{Content: "hi", Deleted: false},
		},
		{
			name: "string dates and null content",
			raw:  `{"content":null,"deleted":false,"creationDate":"12.5","modificationDate":"99.25"}`,
			want: Note{Content: "", Deleted: false, CreationDate: 12.5, ModificationDate: 99.25},
		},
		{
			name: "missing fields",
			raw:  `{}`,
			want: Note{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Note
			if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(got.Tags) == 0 {
				got.Tags = nil
			}
			if got.Content != tc.want.Content || got.Deleted != tc.want.Deleted ||
				got.CreationDate != tc.want.CreationDate || got.ModificationDate != tc.want.ModificationDate {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestGetVersionSendsVersionQuery(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"content":"old","systemTags":["pinned"]}`))
	}))
	defer srv.Close()

	c := New("app", "key", "tok")
	c.APIHost = srv.URL
	n, err := c.GetVersion("abc", 4)
	if err != nil {
		t.Fatal(err)
	}
	if q, _ := url.ParseQuery(gotQuery); q.Get("v") != "4" {
		t.Fatalf("query = %q, want v=4", gotQuery)
	}
	if n.Content != "old" || len(n.SystemTags) != 1 || n.SystemTags[0] != "pinned" {
		t.Fatalf("unexpected note: %+v", n)
	}
}

func TestNoteMarshalStaysBoolean(t *testing.T) {
	b, err := json.Marshal(Note{Content: "x", Deleted: true})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["deleted"] != true {
		t.Fatalf("deleted should marshal as a JSON boolean, got %T %v", m["deleted"], m["deleted"])
	}
}

// A response cut short mid-body used to be parsed as-is: json.Unmarshal then
// reported a baffling "invalid character ' ' in string escape code" and the
// whole sync aborted. It must be recognized as a transport failure and
// retried.
func TestIndexRetriesTruncatedResponse(t *testing.T) {
	old := retryDelay
	retryDelay = 0
	defer func() { retryDelay = old }()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/note/index") {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) == 1 {
			// Announce 100 bytes but send only a prefix that ends right after
			// a backslash, exactly like a dropped connection.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"current":"1","index":[{"id":"a","v":1,"d":{"content":"x\`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"current": "2",
			"index": []any{
				map[string]any{"id": "a", "v": 1, "d": map[string]any{"content": "hello"}},
			},
		})
	}))
	defer srv.Close()

	c := New("app", "key", "tok")
	c.APIHost = srv.URL

	notes, cursor, err := c.Index("")
	if err != nil {
		t.Fatalf("Index should have recovered by retrying: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls.Load())
	}
	if len(notes) != 1 || notes[0].Note.Content != "hello" || cursor != "2" {
		t.Fatalf("unexpected result: notes=%+v cursor=%q", notes, cursor)
	}
}

// A 500 is transient and retried; a 400 is not worth retrying.
func TestGetJSONRetryPolicy(t *testing.T) {
	old := retryDelay
	retryDelay = 0
	defer func() { retryDelay = old }()

	cases := []struct {
		name      string
		status    int
		wantCalls int32
		wantErr   bool
	}{
		{"server error is retried", http.StatusInternalServerError, maxAttempts, true},
		{"rate limit is retried", http.StatusTooManyRequests, maxAttempts, true},
		{"bad request is not retried", http.StatusBadRequest, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := New("app", "key", "tok")
			c.APIHost = srv.URL
			err := c.getJSON(srv.URL+"/x", nil)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Fatalf("attempts = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestIndexRejectsRepeatedPaginationMark(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/note/index") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"current": "1",
			"mark":    "stuck",
			"count":   0,
			"index":   []any{},
		})
	}))
	defer srv.Close()

	c := New("app", "key", "tok")
	c.APIHost = srv.URL

	if _, _, err := c.Index(""); err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("expected a repeated-mark error, got %v", err)
	}
}
