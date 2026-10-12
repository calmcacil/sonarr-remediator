package sonarr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQueueContractsAndBasePath(t *testing.T) {
	for _, mode := range []string{"complete", "duplicate", "changing", "null", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/sonarr/api/v3/queue" || r.URL.Query().Get("includeUnknownSeriesItems") != "true" || r.URL.Query().Get("sortKey") != "timeleft" || r.Header.Get("X-Api-Key") != "key" {
					t.Error("wrong request contract")
				}
				if mode == "null" {
					fmt.Fprint(w, "null")
					return
				}
				if mode == "malformed" {
					fmt.Fprint(w, `{"page":1}`)
					return
				}
				n := 1
				if r.URL.Query().Get("page") == "2" {
					n = 2
				}
				total := 101
				if mode == "changing" && n == 2 {
					total = 102
				}
				records := []QueueRow{}
				if n == 1 {
					for id := 1; id <= 100; id++ {
						records = append(records, QueueRow{ID: id, DownloadID: fmt.Sprintf("download-%d", id)})
					}
				} else {
					id := 101
					if mode == "duplicate" {
						id = 1
					}
					records = append(records, QueueRow{ID: id, DownloadID: fmt.Sprintf("download-%d", id)})
				}
				json.NewEncoder(w).Encode(map[string]any{"page": n, "pageSize": 100, "totalRecords": total, "records": records})
			}))
			defer srv.Close()
			c, _ := New(srv.URL+"/sonarr", "key", time.Second, true)
			rows, e := c.Queue(context.Background())
			if mode == "complete" {
				if e != nil || len(rows) != 101 || calls != 2 {
					t.Fatalf("%d rows %v %d calls", len(rows), e, calls)
				}
			} else if e == nil {
				t.Fatal("unsafe read accepted")
			}
		})
	}
}
func TestMutationTrapRedirectsAndErrors(t *testing.T) {
	mutations := 0
	leaked := false
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer dest.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations++
		}
		http.Redirect(w, r, dest.URL, 302)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "SECRET", time.Second, true)
	if _, e := c.Submit(context.Background(), SearchCommand{Name: "EpisodeSearch"}); e == nil {
		t.Fatal("dry-run command passed")
	}
	if e := c.Remove(context.Background(), 1, true); e == nil {
		t.Fatal("dry-run delete passed")
	}
	if mutations != 0 {
		t.Fatal("mutation reached network")
	}
	if _, e := c.Version(context.Background()); e == nil {
		t.Fatal("redirect accepted")
	}
	if leaked {
		t.Fatal("credential redirect followed")
	}
	errorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, "SECRET private response")
	}))
	defer errorSrv.Close()
	c, _ = New(errorSrv.URL, "SECRET", time.Second, true)
	_, e := c.Version(context.Background())
	if e == nil || !strings.Contains(e.Error(), "authentication") || strings.Contains(e.Error(), "SECRET") {
		t.Fatal("authentication classification/redaction failed")
	}
}
func TestBoundedJSONAndResourceIdentity(t *testing.T) {
	for _, body := range []string{"null", "", `{"id":2}`, strings.Repeat(" ", MaxResponse) + `{}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		c, _ := New(srv.URL, "key", time.Second, true)
		if _, e := c.Episode(context.Background(), 1); e == nil {
			t.Fatal("unsafe resource accepted")
		}
		srv.Close()
	}
}
